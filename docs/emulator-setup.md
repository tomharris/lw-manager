# Emulator setup (Linux)

The device plane is an Android emulator on the dev box. `adb` is identical
against an emulator and a physical handset, so `ADBTransport` is unchanged if
hardware arrives later.

Everything below lives under `$HOME` and needed no root — **except KVM**.

## What is installed

| Component | Location | Notes |
|---|---|---|
| platform-tools (adb) | `~/Android/platform-tools` | symlinked into `~/.local/bin` |
| cmdline-tools | `~/Android/cmdline-tools/latest` | sdkmanager, avdmanager |
| JDK 21 | `~/.local/jdk/jdk-21*` | sdkmanager needs 17+; Pop!_OS ships 11 |
| emulator + system image | `~/Android/emulator`, `~/Android/system-images` | Android 35, `google_apis_playstore`, x86_64 |

Put them on PATH with:

```bash
source scripts/android-env.sh
```

The **Play Store** image was chosen over plain `google_apis` so Last War can be
installed normally rather than sideloaded. Play Store images cannot be rooted,
which costs us nothing: the whole design is screen-in / touch-out, and never
needs root.

## KVM — the one step needing root

Without `/dev/kvm` the emulator falls back to full CPU emulation and is far too
slow to drive. Check:

```bash
ls -l /dev/kvm
```

If it is missing:

```bash
sudo modprobe kvm_amd          # or kvm_intel on Intel
echo kvm_amd | sudo tee /etc/modules-load.d/kvm.conf   # persist across reboots
```

### If modprobe fails with "Invalid argument"

That means SVM is disabled in firmware, not that the module is missing.
Confirm with:

```bash
journalctl -k | grep -i svm
# SVM disabled (by BIOS) in MSR_VM_CR
```

The firmware has set the SVMDIS bit in `MSR_VM_CR`, which hard-locks
virtualization. There is no kernel-side workaround — it needs a firmware
change and a reboot.

**On this machine (System76 Thelio, Gigabyte board, BIOS `F61a`):**

1. Reboot and hold **Del** to enter BIOS setup
2. **M.I.T.** → **Advanced Frequency Settings** → **Advanced CPU Core Settings**
3. Set **SVM Mode** → **Enabled**
4. Save and exit

On other hardware the setting is usually Advanced → CPU Configuration → **SVM
Mode** (AMD) or **VT-x / Intel Virtualization Technology** (Intel).

After rebooting, `kvm_amd` should load automatically and `/dev/kvm` will exist.

Some systems also need the invoking user in the `kvm` group:

```bash
sudo usermod -aG kvm "$USER"   # log out and back in to take effect
```

Confirm acceleration is working before trying to boot:

```bash
source scripts/android-env.sh
emulator -accel-check     # must not say "/dev/kvm is not found"
```

## Creating and running an AVD

The `lastwar` AVD already exists at `~/.android/avd/lastwar.avd`. To recreate:

```bash
source scripts/android-env.sh
avdmanager create avd -n lastwar -k "system-images;android-35;google_apis_playstore;x86_64" -d pixel_6
```

Its `config.ini` is tuned past the stingy defaults, which matter for a 3D
game — the stock 2 GB RAM and 2 GB data partition are not enough:

| Setting | Default | Tuned |
|---|---|---|
| `hw.ramSize` | 2 GB | 4096 |
| `vm.heapSize` | 228M | 576 |
| `disk.dataPartition.size` | 2G | 8192M |
| `hw.gpu.enabled` / `hw.gpu.mode` | no / auto | yes / host |

Screen is 1080×2400 at density 420 (Pixel 6), a mainstream phone resolution —
a reasonable reference for the template library the vision milestone builds.

```bash
# -no-snapshot-load gives a clean boot; drop it for faster restarts later
emulator -avd lastwar -no-snapshot-load &

adb wait-for-device
adb devices          # should list emulator-5554
```

Then install Last War from the Play Store inside the emulator and sign in to an
**alt account** — never a main. See the ToS note in `CLAUDE.md`.

## Registering it with the platform

```bash
./bin/agent devices                                        # confirm the probe
./bin/agent register --nickname myalt --role alliance_data
./bin/agent capture --account <id printed above>
```

## Known risks

- **ARM-only APKs.** Last War may ship arm64 binaries. Android 11+ x86_64
  images include ARM translation, but if the game refuses to install or
  crashes on launch, that is the first thing to suspect. The fallback is an
  arm64 system image (much slower on x86 hardware) or a physical device.
- **Emulator fingerprints** are the most detectable signal in this design.
  Fine for development; prefer physical devices for anything long-running.
