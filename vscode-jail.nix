{
  lib,
  coreutils,
  writeShellScriptBin,
  writeText,
  jaild,
  vscode,
  extraRwDirs ? [ ],
  extraRoDirs ? [ ],
  extHostHook ? "",
}:

let
  jailedExtHost = writeShellScriptBin "vscode-ext-host" ''
    set -euo pipefail

    export PATH="${
      lib.makeBinPath [
        coreutils
        jaild
      ]
    }:$PATH"
    HOST_HOME="$(realpath "$HOME")"
    CWD="''${VSCODE_JAIL_CWD:+$(realpath -e "$VSCODE_JAIL_CWD" 2>/dev/null || true)}"

    if [[ -z "$CWD" || "$CWD" == "/" || "$HOST_HOME/" == "$CWD/"* ]]; then
      JAIL_NAME="code-scratch"
      EXPOSE_CWD=""
    elif [[ "$CWD" == "$HOST_HOME"/* ]]; then
      REL="h-''${CWD#"$HOST_HOME"/}"
      JAIL_NAME="code-''${REL//\//-}"
      EXPOSE_CWD="$CWD"
    else
      REL="''${CWD#/}"
      JAIL_NAME="code-''${REL//\//-}"
      EXPOSE_CWD="$CWD"
    fi

    RW_DIRS=(
      "$HOME/.config/Code/logs"
      "$HOME/.config/Code/User/globalStorage"
      "$HOME/.config/Code/User/workspaceStorage"
      "$HOME/.local/share/direnv"
      "$HOME/.cache/direnv"
      "$HOME/.cache/nix"
      ${lib.concatMapStringsSep "\n      " (d: "\"${d}\"") extraRwDirs}
    )
    mkdir -p "''${RW_DIRS[@]}"

    RO_DIRS=(
      "$HOME/.vscode/extensions"
      "$HOME/.config/Code/CachedData"
      "$HOME/.config/direnv"
      ${lib.concatMapStringsSep "\n      " (d: "\"${d}\"") extraRoDirs}
    )

    BIND_ARGS=()
    for d in "''${RW_DIRS[@]}"; do
      BIND_ARGS+=(--rw-bind "$d")
    done
    for d in "''${RO_DIRS[@]}"; do
      BIND_ARGS+=(--ro-bind "$d")
    done
    if [[ -n "$EXPOSE_CWD" ]]; then
      BIND_ARGS+=(--rw-bind "$EXPOSE_CWD")
    fi

    jail new "$JAIL_NAME" \
      --if-not-exists \
      --user "$(id -un)" \
      --network \
      --nix-mode closure \
      --stub \
      --pkg "$1" \
      --env ELECTRON_RUN_AS_NODE=1 \
      --env ELECTRON_ENABLE_LOGGING=1 \
      --pass-env ELECTRON_RUN_AS_NODE \
      --pass-env LD_LIBRARY_PATH \
      --pass-env LOCALE_ARCHIVE \
      --pass-env TZDIR \
      --pass-env XDG_DATA_DIRS \
      --pass-env 'VSCODE_*' \
      "''${BIND_ARGS[@]}" >&2 3>&-

    if [[ -n "$EXPOSE_CWD" ]]; then
      cd "$EXPOSE_CWD"
    fi

    exec jail run "$JAIL_NAME" -- "$@"
  '';

  # Electron's utilityProcess.fork cannot execute a bwrap wrapper binary and
  # Chromium MessagePorts cannot cross a non-utilityProcess boundary. We spawn
  # the jailed extension host via child_process.spawn with a socketpair on fd 3
  # and bridge the renderer's MessagePort over length-prefixed frames on fd 3.
  extHostJailBridge = writeText "extHostJailBridge.js" ''
    import { spawn } from "node:child_process";
    import { Module } from "node:module";
    import { Socket } from "node:net";

    function attachFramedSocket(sock, onFrame, onClose) {
      let buf = Buffer.alloc(0);
      sock.on("data", chunk => {
        buf = buf.length ? Buffer.concat([buf, chunk]) : chunk;
        while (buf.length >= 4) {
          const len = buf.readUInt32BE(0);
          if (buf.length < 4 + len) break;
          const frame = new Uint8Array(buf.buffer, buf.byteOffset + 4, len).slice();
          buf = buf.subarray(4 + len);
          onFrame(frame);
        }
      });
      sock.on("close", onClose);
      sock.on("error", onClose);
      return u8 => {
        const hdr = Buffer.allocUnsafe(4);
        hdr.writeUInt32BE(u8.byteLength, 0);
        sock.write(hdr);
        sock.write(Buffer.from(u8.buffer, u8.byteOffset, u8.byteLength));
      };
    }

    export function spawnJailedExtHost(self, conf, modulePath, args, env) {
      const win = self.windowsMainService?.getWindowById(conf.responseWindowId);
      const cwd = win?.openedWorkspace?.uri?.fsPath || "";
      const cp = spawn(
        "${lib.getExe jailedExtHost}",
        [process.execPath, ...(conf.execArgv ?? []), modulePath, ...args],
        {
          env: { ...env, VSCODE_JAIL_CWD: cwd },
          stdio: ["ignore", "pipe", "pipe", "pipe"],
        }
      );
      cp.postMessage = (_msg, ports) => {
        const port = ports?.[0];
        const sock = cp.stdio[3];
        if (!port || !sock) return;
        const sendFrame = attachFramedSocket(
          sock,
          frame => port.postMessage(frame),
          () => port.close()
        );
        port.on("message", ev => sendFrame(ev.data));
        port.on("close", () => sock.destroy());
        port.start();
      };
      return cp;
    }

    export function installExtHostParentPort() {
      if (process.parentPort || process.env.VSCODE_WILL_SEND_MESSAGE_PORT !== "1") return;
      ${extHostHook}
      process.parentPort = {
        on(ev, cb) {
          if (ev !== "message") return;
          const sock = new Socket({ fd: 3, readable: true, writable: true });
          const handlers = { message: [], close: [] };
          const sendFrame = attachFramedSocket(
            sock,
            data => { for (const h of handlers.message) h({ data }); },
            () => { for (const h of handlers.close) h(); }
          );
          cb({
            ports: [{
              on(e, h) { handlers[e]?.push(h); },
              start() {},
              postMessage: sendFrame,
            }],
          });
        },
      };
    }
  '';
in
vscode.overrideAttrs (old: {
  postPatch = (old.postPatch or "") + ''
    outDir=resources/app/out
    cp ${extHostJailBridge} "$outDir/extHostJailBridge.js"

    mainFile="$outDir/mainImpl.js"
    [[ -f "$mainFile" ]] || mainFile="$outDir/main.js"
    sed -E -i \
      -e '1s|^|import{spawnJailedExtHost}from"./extHostJailBridge.js";|' \
      -e 's|this\.process=([A-Za-z0-9_\$]+\.fork\(r,o,\{serviceName:t,env:([A-Za-z0-9_\$]+),execArgv:s,allowLoadingUnsignedLibraries:a,respondToAuthRequestsFromMainProcess:c,stdio:d\}\))|this.process=e.type==="extensionHost"?spawnJailedExtHost(this,e,r,o,\2):\1|' \
      "$mainFile"
    grep -qE 'spawnJailedExtHost\(this,e,r,o,[A-Za-z0-9_\$]+\)' "$mainFile"

    sed -E -i \
      -e '1s|^|import{installExtHostParentPort}from"./extHostJailBridge.js";|' \
      -e 's|await ([A-Za-z0-9_\$]+)\(\);await import\(|await \1();installExtHostParentPort();await import(|' \
      "$outDir/bootstrap-fork.js"
    grep -qF 'installExtHostParentPort();await import(' "$outDir/bootstrap-fork.js"

    ehp="$outDir/vs/workbench/api/node/extensionHostProcess.js"
    sed -E -i \
      "s|Extension host with pid [$]\{this\._hostUtils\.pid\} started|& (bwrap jail root: \''${process.cwd()})|" \
      "$ehp"
    grep -qF "started (bwrap jail root: \''${process.cwd()})" "$ehp"
    ehpHash=$(sha256sum "$ehp" | cut -d' ' -f1 | tr 'a-f' 'A-F' | basenc --base16 -d | basenc --base64 | tr -d '=\n')
    sed -E -i \
      "s|(\"vs/workbench/api/node/extensionHostProcess\.js\":[[:space:]]*\")[^\"]+\"|\1$ehpHash\"|" \
      resources/app/product.json
    grep -qF "$ehpHash" resources/app/product.json
  '';
})
