{
  pkgs,
  nixosModule,
  interactiveWeb ? false,
  webPort ? 7480,
}:

pkgs.testers.runNixOSTest {
  name = if interactiveWeb then "jaild-web-vm" else "jaild-integration";

  nodes.machine =
    { config, lib, pkgs, ... }:
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
        jq
        iproute2
      ];

      virtualisation = {
        cores = 4;
        memorySize = 2048;
        additionalPaths = [
          pkgs.hello
          pkgs.ripgrep
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

        with subtest("embedded web UI and REST API over socket"):
            html = machine.succeed(as_user("curl -sf --unix-socket /run/user/1000/jaild.sock http://localhost/"))
            assert "<title>jaild" in html
            jails_json = machine.succeed(as_user("curl -sf --unix-socket /run/user/1000/jaild.sock http://localhost/api/jails | jq -r '.[].config.name'"))
            assert "dev" in jails_json and "web" in jails_json
      '';
}
