# Running a donor on Windows (WSL2)

WSL2 works as a donor host. WSL1 does not. There are four things to get right,
and one expectation to set.

Start here, then follow [the donor quickstart](../quickstart/donor.md) inside
your WSL2 shell.

---

## The expectation first

**When Windows sleeps, your node is offline.** WSL2 stops with the host. A
laptop that closes at night is a donor that disappears every night.

That is not fatal — Nova tolerates donors coming and going — but a node that is
down more than it is up gets deprioritised, and repeated long absences make
your operator move your data elsewhere. If this machine sleeps, tell your
operator so they can plan around it, or use a machine that stays on.

---

## 1. Confirm you are on WSL2

```powershell
wsl -l -v
```

Expected: `VERSION` reads `2`.

If it reads `1`:

```powershell
wsl --set-version <distro-name> 2
```

WSL1 cannot work — it lacks the real Linux kernel that the overlay network's
TUN device needs.

---

## 2. Enable systemd

Inside WSL, edit `/etc/wsl.conf`:

```ini
[boot]
systemd=true
```

Then from PowerShell:

```powershell
wsl --shutdown
```

Reopen your WSL shell. Verify:

```sh
systemctl is-system-running
```

Anything other than `offline` is fine — `running` and `degraded` both work.

Without systemd, Docker will not stay running unattended.

---

## 3. Confirm the TUN device

```sh
ls -l /dev/net/tun
```

Expected: `crw-rw-rw- 1 root root 10, 200 ... /dev/net/tun`

If missing:

```sh
sudo modprobe tun
echo tun | sudo tee -a /etc/modules-load.d/modules.conf
```

The overlay network cannot start without it.

---

## 4. Keep storage on the Linux filesystem

Put your donor folder somewhere under your WSL home directory:

```sh
mkdir -p ~/nova-donor
```

**Do not use `/mnt/c` or any other Windows drive.** Cross-filesystem access is
dramatically slower and does not honour Linux file permissions, so your private
keys would not be protected and the storage layer would crawl.

Leave `storage_dir` in `node.yaml` at its default. It refers to a Docker volume
inside the Linux filesystem, which is what you want.

---

## 5. Make Docker persistent

Docker Desktop with the WSL2 backend is the simplest option — enable
integration for your distro in **Settings → Resources → WSL Integration**, and
turn on **Start Docker Desktop when you log in**.

If you installed Docker inside WSL instead:

```sh
sudo systemctl enable --now docker
```

Verify it survives a restart:

```powershell
wsl --shutdown
```

then reopen the shell and run `docker ps`. It should work without you starting
anything.

---

## Then

Follow [the donor quickstart](../quickstart/donor.md) from step 1, inside your
WSL2 shell.

---

## Platform support

| Platform | Status | Notes |
|---|---|---|
| Linux (systemd, x86-64 / arm64) | Supported | The primary target. |
| WSL2 on Windows | Supported | This page. Sleep means offline. |
| WSL1 | **Not supported** | No real kernel; no TUN device. |
| macOS | **Not supported** | Docker Desktop's VM does not expose TUN in the way the overlay requires. |
| Raspberry Pi (64-bit, arm64) | Works | Use decent storage — SD cards wear out under constant writes. |
| Container-only hosts (LXC, some VPS) | Depends | Needs `/dev/net/tun` and `NET_ADMIN`. Many unprivileged LXC containers deny both. Check before committing. |

---

## When something is wrong

**Node will not start, overlay errors:** re-check steps 1 and 3. Nearly every
WSL2 problem is WSL1 or a missing TUN device.

**Everything is very slow:** your folder is probably on `/mnt/c`. Move it under
your WSL home directory.

**Node vanishes overnight:** Windows is sleeping. Change the power settings, or
accept it and tell your operator.
