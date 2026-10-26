# gossh

A terminal UI for your SSH hosts in `~/.ssh/config`: connect, transfer files
(FileZilla-style, two panes with a transfer queue), port forwarding and key
management, all from one screen.

## Install

No Go toolchain needed. The installer picks the right binary for your OS and CPU,
puts `gossh` on your PATH, and creates `~/.ssh/config` and `~/.ssh/keys/`.

**Windows (x64 / ARM64)**, in PowerShell:

```powershell
irm https://raw.githubusercontent.com/JiaBao-do/gossh/main/install.ps1 | iex
```

**Linux / macOS (amd64 / arm64)**:

```sh
curl -fsSL https://raw.githubusercontent.com/JiaBao-do/gossh/main/install.sh | sh
```

Open a new terminal and run `gossh`.

| | Windows | Linux / macOS |
|---|---|---|
| Binary | `%LOCALAPPDATA%\Programs\gossh\gossh.exe` | `~/.local/bin/gossh` |
| PATH | user PATH | `~/.bashrc`, `~/.bash_profile` (macOS), `~/.zshrc`, fish `config.fish` or `~/.profile` |
| Admin / sudo | not needed | not needed |

Options (environment variables, or flags on the script):

| Variable | Flag (sh / ps1) | Meaning |
|---|---|---|
| `GOSSH_VERSION=v1.2.0` | `--version` / `-Version` | install a specific release |
| `GOSSH_INSTALL_DIR=...` | `--dir` / `-InstallDir` | install somewhere else |
| `GOSSH_UNINSTALL=1` | `--uninstall` / `-Uninstall` | remove gossh and its PATH entry |
| `GOSSH_NO_MODIFY_PATH=1` | | leave PATH alone |

**Offline install:** download the release files (the binary for your platform
plus `install.sh` / `install.ps1`) into one folder and run the script there. It
uses the binary next to it instead of downloading.

## Usage

Run `gossh`. Everything happens from its host list; you don't need to
remember any commands. After each action you come back to the list, with a
line at the bottom saying what happened.

```
enter   connect                     a   add a host
t       transfer files              e   edit the selected host
s       transfer files as root      d   delete the selected host
f       port forwarding             k   keys (import, move into ~/.ssh/keys)
/       filter the list             u   upgrade gossh
?       show all keys               q   quit
```

A few shortcuts still work from the shell:

```
gossh prod-db            connect straight to a saved host (any ssh flags pass through)
gossh user@1.2.3.4       plain ssh
gossh upgrade            install the latest release (same as u in gossh)
gossh version
```

### Hosts and keys

`a` and `e` open a form for the alias, HostName, User and Port, then a key
picker that lists:

- the keys already in `~/.ssh/keys`
- private keys found in `~/.ssh`, `Downloads` and `Desktop` (e.g. a `.pem`
  you just downloaded)
- **Browse for a key file...** (a file picker) and **Type a path...**

When the key is outside `~/.ssh/keys` you choose **Copy** (keep the
original), **Move** (remove the original) or **Keep** (use it where it is).
Hosts that already point at keys elsewhere keep working and are never
changed until you decide; `k` → *Bring keys my hosts use from other
folders* moves them in one go. A key still used by another host (or a
`Host *` / `Match` block) is never deleted.

Imported keys get owner-only permissions (on Windows the ACL is locked down
too, which OpenSSH requires) and their `.pub` is brought along. The config
references `~/.ssh/keys/<name>`, which works on every OS.

### Port forwarding

Select a host and press `f`. Pick what you want:

- **Access a remote service here** (`-L`): e.g. the server's database on your localhost:5433
- **Expose a local service on the server** (`-R`): e.g. the server's port 9000 reaches your localhost:3000
- **SOCKS proxy** (`-D`): browse through the server

Busy local ports are reported before connecting. Ctrl+C stops the forward
and brings you back to gossh.

### File transfer

Select a host and press `t` (or `s` to work as root through `sudo`, e.g. to
edit files under `/etc`). Transfers use SFTP over your system `ssh`, so
everything in `~/.ssh/config` (keys, agents, `ProxyJump`, password
prompts) works the same as connecting. The server only needs SFTP enabled,
which is the OpenSSH default.

The left pane is this computer, the right pane is the host, and the
**Transfers** panel underneath shows every copy with its progress.

- **Copy a file or a whole folder:** put the cursor on it and press `c`.
  It goes into the folder open in the other pane; folders are copied with
  everything inside.
- **Copy several things:** mark them with `space` (they turn yellow), then `c`.
- **Copy only what is inside a folder:** open the folder, press `a` to mark
  everything, then `c`.
- **If a file already exists** you choose: `o` overwrite, `n` overwrite only
  if newer, `s` skip, `r` keep both (saved as `file (1).txt`). Shift + the
  letter applies the answer to the rest of the queue.

Copies are written under a temporary name and swapped in at the end, so a
cancelled or broken transfer never damages the file that was already there.
One failed file doesn't stop the rest: the Transfers panel marks it, `enter`
shows why and `r` retries.

```
tab        pane / transfers panel   space      mark / unmark (a = all)
enter / →  open folder              c / F5     copy to the other pane
⌫ / ←      parent folder            d / F8     delete       r / F2   rename
g          go to a path             m / F7     new folder   .        show hidden
~          home folder              esc        cancel the running transfer
?          help (inside the file manager)
```

With `s` (sudo), passwordless sudo is used when available; otherwise gossh
asks for the sudo password. Files written this way are owned by root. Hosts
whose sudoers has `requiretty` are not supported.

### Upgrading

Press `u` in gossh (or run `gossh upgrade`). It downloads the latest release
for your OS/CPU from GitHub, checks it against the published checksums and
replaces itself. Coming from v1.1.0 or older, run the install command once
more; those versions don't have the upgrade command yet.

### Your existing ~/.ssh/config is safe

gossh only edits `Host <alias>` blocks and only the HostName, User, Port and
IdentityFile lines in them. Everything else (`Include`, `Host *`, `Match`,
`ProxyJump`, `LocalForward`, comments, formatting) is kept exactly as is,
and the previous file is saved to `~/.ssh/config.bak` before every change.

## Development

Requires Go 1.27.3+, GNU make and `sh` (Git Bash on Windows).

```
make                 build bin/gossh for this machine
make run ARGS=list   go run with arguments
make sandbox         run with HOME set to ./.sandbox (your real ~/.ssh is untouched)
make tools           install Delve
make debug           headless Delve on :2345; attach with `dlv connect :2345` or your IDE
make debug-build     unoptimised binary for your debugger
make install         build and install this checkout onto PATH
make uninstall
make dist            cross-compile all 6 platforms + checksums into dist/
make test vet fmt tidy upgrade clean
```

### Releasing

```
git tag v1.0.0 && git push origin v1.0.0
```

The `release` workflow builds every platform and publishes a GitHub release,
which is what the install scripts download.
