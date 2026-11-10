# jaild

Rootless `bubblewrap` jail manager for NixOS with live bind-mount and Nix closure injection into running jails.

## NixOS Setup

```nix
{
  imports = [ inputs.jaild.nixosModules.default ];
  services.jaild.enable = true;
}
```

`jaild` runs on demand as a socket-activated user service (`$XDG_RUNTIME_DIR/jaild.sock`). Persisted jail definitions live in `~/.local/share/jaild/jails/<name>.toml`.

## CLI Usage

### Create a jail

```sh
jail new dev --network --gpu --gui --home-src ~/jails/dev-home --pkg nixpkgs#git --pkg nixpkgs#ripgrep
```

Flags for `jail new <name> [flags] [-- default-cmd...]`:
- `--network` - share host network
- `--gpu` - expose `/dev/dri`, `/dev/nvidia*`, and OpenGL/CUDA drivers
- `--gui` - expose X11 socket and `XAUTHORITY`
- `--nix-mode closure|full` - mount only required store closures (default: `closure`) or all of `/nix/store` (`full`)
- `--home-src <dir>` - bind a persistent host directory as `$HOME` (default: ephemeral `tmpfs`)
- `--pkg <installable|path>` - include Nix packages in the jail (repeatable)
- `--dbus-talk <name>` - allow talking to a session D-Bus service via `xdg-dbus-proxy` (repeatable)
- `--pass-env <VAR>` - pass a host environment variable into the jail (repeatable)

### Start a program or shell inside a jail

```sh
# Start an interactive shell (or the jail's default command)
jail run dev

# Run a specific command inside the jail (multiple instances share the same live stage)
jail run dev -- nvim /work/project
```

### Expose and hide host paths on the fly

Changes take effect immediately inside already-running `jail run` processes:

```sh
# Mirror /mnt/datasets into /mnt/datasets read-only
jail expose dev /mnt/datasets --ro

# Expose ~/src/project at /work/project read-write
jail expose dev ~/src/project --target /work/project

# Unmount from the jail (by target path)
jail hide dev /work/project
```

### Add Nix packages on the fly

```sh
jail pkg dev nixpkgs#htop nixpkgs#jq
```

### Inspect and manage jails

```sh
jail ls              # List jails and stage state
jail status dev      # Print full JSON configuration and status
jail stop dev        # Unmount the stage (keeps configuration)
jail del dev         # Unmount the stage and delete configuration
```

### Web UI

```sh
jail web --listen 127.0.0.1:7480
```
