# Source this to put adb, the emulator, and a JDK 17+ on PATH.
#
#   source scripts/android-env.sh
#
# Everything lives under $HOME — no root was needed for any of it. The one
# thing that does need root is KVM (see docs/emulator-setup.md).

export ANDROID_HOME="${ANDROID_HOME:-$HOME/Android}"
export ANDROID_SDK_ROOT="$ANDROID_HOME"

# sdkmanager and avdmanager require JDK 17+; Pop!_OS ships 11 as the system
# default, so we point at a home-directory JDK rather than changing the system.
for jdk in "$HOME"/.local/jdk/jdk-*; do
    if [ -x "$jdk/bin/java" ]; then
        export JAVA_HOME="$jdk"
        break
    fi
done

export PATH="$JAVA_HOME/bin:$ANDROID_HOME/platform-tools:$ANDROID_HOME/emulator:$ANDROID_HOME/cmdline-tools/latest/bin:$PATH"
