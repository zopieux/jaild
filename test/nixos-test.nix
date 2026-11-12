{
  pkgs,
  nixosModule,
  interactiveWeb ? false,
  webPort ? 7480,
}:

let
  hmGitConfig = pkgs.writeText "hm_gitconfig" "[user]\n\tname = Test User\n";
  hmGitIgnore = pkgs.writeText "hm_gitignore" ".direnv/\n";
  hmSecret = pkgs.writeText "hm_secret" "top-secret-token\n";
  hmFiles = pkgs.runCommand "home-manager-files" { } ''
    mkdir -p $out/.config/git
    ln -s ${hmGitConfig} $out/.config/git/config
    ln -s ${hmGitIgnore} $out/.config/git/ignore
    ln -s ${hmSecret} $out/.secret-token
  '';
in
pkgs.testers.runNixOSTest {
  name = if interactiveWeb then "jaild-web-vm" else "jaild-integration";

  nodes.machine =
    { config, lib, pkgs, ... }:
    let
      patchedVscode = config.services.jaild.package.patchVscode {
        inherit lib;
        inherit (pkgs)
          coreutils
          writeShellScriptBin
          writeText
          vscode
          ;
        jaild = config.services.jaild.package;
      };
    in
    {
      imports = [ nixosModule ];

      networking = {
        useDHCP = false;
        firewall.enable = false;
        defaultGateway = "10.0.2.2";
        nameservers = [ "10.0.2.3" ];
        interfaces.eth0.ipv4.addresses = [
          {
            address = "10.0.2.15";
            prefixLength = 24;
          }
        ];
      };

      nix.settings.experimental-features = [
        "nix-command"
        "flakes"
      ];
      nix.nixPath = [ "nixpkgs=${pkgs.path}" ];
      nix.registry.nixpkgs.to = {
        type = "path";
        path = pkgs.path;
      };

      services.jaild.enable = true;

      users.users.someuser = {
        isNormalUser = true;
        uid = 1000;
        linger = true;
      };

      environment.systemPackages = with pkgs; [
        curl
        direnv
        gitMinimal
        jq
        iproute2
        patchedVscode
        xorg-server
      ];

      virtualisation = {
        cores = 4;
        memorySize = 3072;
        additionalPaths = [
          hmFiles
          pkgs.hello
          pkgs.ripgrep
          pkgs.tree
          pkgs.vscode-extensions.mkhl.direnv
          (pkgs.mkShellNoCC {
            packages = [
              pkgs.hello
              pkgs.tree
            ];
          }).inputDerivation
        ];
        forwardPorts = lib.optionals interactiveWeb [
          {
            from = "host";
            host.address = "127.0.0.1";
            host.port = webPort;
            guest.port = 7480;
          }
        ];
      };
    };

  testScript =
    if interactiveWeb then
      ''
        import shlex
        import signal
        import threading

        def as_user(cmd: str) -> str:
            inner = f"export XDG_RUNTIME_DIR=/run/user/1000; {cmd}"
            return f"su - someuser -c {shlex.quote(inner)}"

        serial_stdout_off()
        start_all()
        machine.wait_for_unit("multi-user.target")
        machine.wait_for_unit("user@1000.service")

        machine.succeed(as_user("nohup jail web --listen 0.0.0.0:7480 >/tmp/jail-web.log 2>&1 &"))
        machine.wait_for_open_port(7480)
        print("\n============================================================")
        print("jaild Web UI available on host at: http://127.0.0.1:${toString webPort}")
        print("Press Ctrl-C to stop the test VM.")
        print("============================================================\n", flush=True)
        stop_event = threading.Event()
        for sig in (signal.SIGINT, signal.SIGTERM):
            signal.signal(sig, lambda _s, _f: stop_event.set())
        stop_event.wait()
      ''
    else
      ''
        from datetime import timedelta
        import shlex

        def as_user(cmd: str) -> str:
            inner = f"export XDG_RUNTIME_DIR=/run/user/1000; {cmd}"
            return f"su - someuser -c {shlex.quote(inner)}"

        serial_stdout_off()
        start_all()
        machine.wait_for_unit("multi-user.target")
        machine.wait_for_unit("user@1000.service")

        with subtest("systemd socket activation: daemon starts only on demand"):
            machine.succeed(as_user("systemctl --user is-active jaild.socket"))
            machine.fail(as_user("systemctl --user is-active jaild.service"))
            machine.succeed(as_user("jail ls"))
            machine.succeed(as_user("systemctl --user is-active jaild.service"))

        with subtest("create jail, verify TOML persistence and stage protections"):
            machine.succeed(as_user("jail new dev"))
            machine.succeed(as_user("test -f ~/.local/share/jaild/jails/dev.toml"))
            # Root skeleton must be read-only while /tmp and $HOME are writable
            machine.succeed(as_user(
                "jail run dev -- sh -ec '"
                "test $(id -u) -eq 1000 && "
                "touch /tmp/ok && "
                "touch $HOME/ok && "
                "(! touch /tamper 2>/dev/null)'"
            ))

        with subtest("live dynamic expose (rw and ro) and hide into running jail"):
            machine.succeed(as_user(
                "mkdir -p /tmp/host-rw /tmp/host-ro && "
                "echo initial-rw > /tmp/host-rw/data && "
                "echo secret-ro > /tmp/host-ro/data"
            ))

            # Launch a background jailed worker waiting for dynamic mounts
            machine.succeed(as_user(
                "nohup jail run dev -- sh -ec '"
                "while [ ! -f /tmp/host-rw/data ]; do sleep 0.05; done; "
                "echo updated-by-jail > /tmp/host-rw/data; "
                "while [ ! -f /tmp/host-ro/data ]; do sleep 0.05; done; "
                "cat /tmp/host-ro/data > /tmp/seen-ro; "
                "if echo hack > /tmp/host-ro/data 2>/dev/null; then echo RO_BROKEN > /tmp/ro-status; else echo RO_OK > /tmp/ro-status; fi; "
                "while [ -e /tmp/host-rw/data ]; do sleep 0.05; done; "
                "echo HIDDEN_OK > /tmp/hide-status"
                "' >/tmp/jail-worker.log 2>&1 &"
            ))

            machine.succeed(as_user("jail expose dev /tmp/host-rw --rw"))
            machine.succeed(as_user("jail expose dev /tmp/host-ro"))
            machine.wait_until_succeeds(
                as_user("grep -q updated-by-jail /tmp/host-rw/data"),
                timeout=timedelta(seconds=10),
            )
            machine.wait_until_succeeds(
                as_user("jail run dev -- sh -ec 'test \"$(cat /tmp/ro-status 2>/dev/null)\" = RO_OK && test \"$(cat /tmp/seen-ro 2>/dev/null)\" = secret-ro'"),
                timeout=timedelta(seconds=10),
            )

            # Hide /tmp/host-rw and verify running jail sees it vanish
            machine.succeed(as_user("jail hide dev /tmp/host-rw"))
            machine.wait_until_succeeds(
                as_user("jail run dev -- test -f /tmp/hide-status"),
                timeout=timedelta(seconds=10),
            )

        with subtest("per-jail peer group isolation"):
            machine.succeed(as_user("jail new web"))
            # /tmp/host-ro is still exposed in dev, but must not be visible in web
            machine.succeed(as_user("jail run dev -- test -f /tmp/host-ro/data"))
            machine.succeed(as_user("jail run web -- test ! -e /tmp/host-ro/data"))

        with subtest("nix closure isolation and dynamic package injection"):
            hello_bin = "${pkgs.hello}/bin/hello"
            # Unreferenced store path must not exist inside closure-mode jail initially
            machine.succeed(as_user(f"jail run dev -- test ! -e {hello_bin}"))
            # Dynamically inject pkgs.hello closure into running dev jail
            machine.succeed(as_user("jail pkg dev ${pkgs.hello}"))
            out = machine.succeed(as_user("jail run dev -- hello"))
            assert "Hello, world!" in out, f"unexpected hello output: {out}"
            # Still hidden in web jail
            machine.succeed(as_user(f"jail run web -- test ! -e {hello_bin}"))

        with subtest("re-running jail new on existing jail overwrites config and remounts stage"):
            hello_bin = "${pkgs.hello}/bin/hello"
            machine.succeed(as_user("jail new dev --user customuser"))
            out = machine.succeed(as_user("jail run dev -- sh -ec 'echo $USER:$HOME'")).strip()
            assert out == "customuser:/home/customuser", f"unexpected user/home: {out}"
            machine.succeed(as_user("jail run dev -- test ! -e /tmp/host-ro/data"))
            machine.succeed(as_user(f"jail run dev -- test ! -e {hello_bin}"))

        with subtest("in-jail nix-shell stub works with --stub (default) and fails with --stub=false"):
            rg_bin = "${pkgs.ripgrep}/bin/rg"
            machine.succeed(as_user(f"jail run dev -- test ! -e {rg_bin}"))
            out = machine.succeed(as_user("jail run dev -- nix-shell -p ripgrep --run 'rg --version'"))
            assert "ripgrep" in out, f"unexpected rg output: {out}"

            machine.succeed(as_user("jail new nostub --stub=false"))
            machine.fail(as_user("jail run nostub -- nix-shell -p hello --run hello"))

        with subtest("nefarious in-jail attempts against stub binary and socket"):
            machine.succeed(as_user(
                "mkdir -p /tmp/host-secret && "
                "echo top-secret > /tmp/host-secret/flag && "
                "cat > /tmp/host-secret/flake.nix <<'EOF'\n"
                "{\n"
                "  outputs = { self }: {\n"
                "    packages.x86_64-linux.default = builtins.path { path = ./.; name = \"leaked-secret\"; };\n"
                "  };\n"
                "}\n"
                "EOF"
            ))
            machine.succeed(as_user("jail run dev -- test ! -e /run/user/1000/jaild.sock"))
            machine.succeed(as_user(
                "jail run dev -- nix-shell -p curl --run '"
                "code=$(curl -s -o /dev/null -w \"%{http_code}\" --unix-socket /run/user/1000/jaild-stub.sock "
                "-X POST http://localhost/api/jails/dev/expose "
                "-d \"{\\\"path\\\":\\\"/tmp/host-secret\\\",\\\"target\\\":\\\"/mnt\\\",\\\"rw\\\":true}\"); "
                "test \"$code\" = 404'"
            ))
            machine.succeed(as_user("jail run dev -- test ! -e /mnt/flag"))
            machine.fail(as_user("jail run dev -- nix-shell -p 'path:/tmp/host-secret#default' --run true"))
            machine.fail(as_user("jail run dev -- nix-shell -p /tmp/host-secret --run true"))
            machine.succeed(as_user("jail run dev -- sh -c 'test ! -e /nix/store/*-leaked-secret/flag'"))

        with subtest("embedded web UI and REST API over socket"):
            html = machine.succeed(as_user("curl -sf --unix-socket /run/user/1000/jaild.sock http://localhost/"))
            assert "<title>jaild" in html
            jails_json = machine.succeed(as_user("curl -sf --unix-socket /run/user/1000/jaild.sock http://localhost/api/jails | jq -r '.[].config.name'"))
            assert "dev" in jails_json and "web" in jails_json

        with subtest("vscode extension host in jaild closure mode with direnv flake reload"):
            hello_bin = "${pkgs.hello}/bin/hello"
            tree_bin = "${pkgs.tree}/bin/tree"
            ext_json = ${ builtins.toJSON (pkgs.vscode-utils.toExtensionJson [ pkgs.vscode-extensions.mkhl.direnv ]) }

            machine.succeed(as_user(
                "mkdir -p ~/.vscode/extensions ~/.config/Code/User ~/.config/git /tmp/test /home/someuser/dev/proj && "
                "ln -sfn ${hmFiles}/.config/git/config ~/.config/git/config && "
                "ln -sfn ${hmFiles}/.config/git/ignore ~/.config/git/ignore && "
                "ln -sfn ${pkgs.vscode-extensions.mkhl.direnv}/share/vscode/extensions/mkhl.direnv ~/.vscode/extensions/mkhl.direnv && "
                f"printf %s {shlex.quote(ext_json)} > ~/.vscode/extensions/extensions.json && "
                "cat > ~/.config/Code/User/settings.json <<'EOF'\n"
                "{\n"
                "  \"direnv.restart.automatic\": true,\n"
                "  \"security.workspace.trust.enabled\": false,\n"
                "  \"window.restoreWindows\": \"none\",\n"
                "  \"workbench.startupEditor\": \"none\",\n"
                "  \"extensions.autoUpdate\": false,\n"
                "  \"update.mode\": \"none\",\n"
                "  \"telemetry.telemetryLevel\": \"off\"\n"
                "}\n"
                "EOF"
            ))

            machine.succeed(as_user(
                "cat > /home/someuser/dev/proj/flake.nix <<'EOF'\n"
                "{\n"
                "  inputs.nixpkgs.url = \"path:${pkgs.path}\";\n"
                "  outputs = { self, nixpkgs }: {\n"
                "    devShells.x86_64-linux.default = nixpkgs.legacyPackages.x86_64-linux.mkShellNoCC {\n"
                "      packages = [ nixpkgs.legacyPackages.x86_64-linux.hello ];\n"
                "    };\n"
                "  };\n"
                "}\n"
                "EOF\n"
                "nix flake lock /home/someuser/dev/proj && "
                "echo 'use flake' > /home/someuser/dev/proj/.envrc && "
                "direnv allow /home/someuser/dev/proj"
            ))

            machine.succeed(as_user("nohup Xvfb :99 -screen 0 1280x1024x24 >/tmp/xvfb.log 2>&1 &"))
            machine.wait_until_succeeds(as_user("test -S /tmp/.X11-unix/X99"), timeout=timedelta(seconds=10))

            machine.succeed(as_user(
                "DISPLAY=:99 nohup code --no-sandbox --disable-gpu --verbose /home/someuser/dev/proj >/tmp/vscode.log 2>&1 &"
            ))

            machine.wait_until_succeeds(
                as_user(
                    "jail status code-h-dev-proj | jq -e '.active == true and .config.nix_mode == \"closure\"' && "
                    "grep -rq 'started (bwrap jail root: /home/someuser/dev/proj)' ~/.config/Code/logs/ && "
                    "grep -rq '${pkgs.hello}/bin' ~/.config/Code/logs/"
                ),
                timeout=timedelta(seconds=60),
            )

            machine.succeed(as_user("jail run code-h-dev-proj -- head -n1 /etc/ssl/certs/ca-bundle.crt"))
            machine.succeed(as_user("jail run code-h-dev-proj -- head -n1 /etc/ssl/certs/ca-certificates.crt"))
            machine.succeed(as_user("jail run code-h-dev-proj -- head -n1 /etc/pki/tls/certs/ca-bundle.crt"))
            machine.succeed(as_user("jail run code-h-dev-proj -- cat ~/.config/git/config | grep -q 'Test User'"))
            machine.succeed(as_user("jail run code-h-dev-proj -- cat ~/.config/git/ignore | grep -q '.direnv/'"))
            machine.succeed(as_user("jail run code-h-dev-proj -- test ! -e ${hmFiles}/.secret-token"))
            machine.succeed(as_user("jail run code-h-dev-proj -- nix-shell -p curl --run 'curl -fsSIm 4 https://google.com'"))
            machine.succeed(as_user(f"jail run code-h-dev-proj -- {hello_bin}"))
            machine.succeed(as_user(f"jail run code-h-dev-proj -- test ! -e {tree_bin}"))

            machine.succeed(as_user(
                "cat > /home/someuser/dev/proj/flake.nix <<'EOF'\n"
                "{\n"
                "  inputs.nixpkgs.url = \"path:${pkgs.path}\";\n"
                "  outputs = { self, nixpkgs }: {\n"
                "    devShells.x86_64-linux.default = nixpkgs.legacyPackages.x86_64-linux.mkShellNoCC {\n"
                "      packages = with nixpkgs.legacyPackages.x86_64-linux; [ hello tree ];\n"
                "    };\n"
                "  };\n"
                "}\n"
                "EOF"
            ))

            machine.wait_until_succeeds(
                as_user(
                    "grep -rq '${pkgs.tree}/bin' ~/.config/Code/logs/ && "
                    f"jail run code-h-dev-proj -- {tree_bin} --version"
                ),
                timeout=timedelta(seconds=60),
            )

            machine.succeed(as_user(
                "cp /home/someuser/dev/proj/flake.nix /home/someuser/dev/proj/flake.lock /home/someuser/dev/proj/.envrc /tmp/test/ && "
                "direnv allow /tmp/test && "
                "DISPLAY=:99 nohup code --no-sandbox --disable-gpu --new-window /tmp/test >/tmp/vscode-tmp.log 2>&1 &"
            ))
            machine.wait_until_succeeds(
                as_user(
                    "jail status code-tmp-test | jq -e '.active == true and .config.nix_mode == \"closure\"' && "
                    "grep -rq 'started (bwrap jail root: /tmp/test)' ~/.config/Code/logs/ && "
                    f"jail run code-tmp-test -- {tree_bin} --version"
                ),
                timeout=timedelta(seconds=60),
            )
      '';
}
