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

If `modprobe` reports **"disabled by BIOS"**, virtualization is off in
firmware. Reboot into BIOS and enable **SVM Mode** (AMD) or **VT-x / Intel
Virtualization Technology** (Intel), usually under Advanced → CPU
Configuration.

Some systems also need the invoking user in the `kvm` group:

```bash
sudo usermod -aG kvm "$USER"   # log out and back in to take effect
```

## Creating and running an AVD

```bash
source scripts/android-env.sh

avdmanager create avd -n lastwar -k "system-images;android-35;google_apis_playstore;x86_64" -d pixel_6

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
