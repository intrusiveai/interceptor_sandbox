"""Native host compiler selection shared by release builders and their tests."""
import os
import platform
import shutil
import subprocess


def native_platform():
    machine = platform.machine().lower()
    architecture = {"x86_64": "amd64", "amd64": "amd64", "aarch64": "arm64", "arm64": "arm64"}.get(machine)
    system = platform.system().lower()
    if system not in ("linux", "darwin") or architecture is None:
        raise ValueError("release builds require a supported native Linux/macOS host")
    return system + "/" + architecture


def native_environment(target):
    if target != native_platform():
        raise ValueError("release platform requires a matching native OS/architecture runner")
    env = dict(os.environ)
    # A caller's cross-compiler/target settings must not change the release.
    for name in ("GOOS", "GOARCH", "CC", "CXX", "CGO_CFLAGS", "CGO_CPPFLAGS",
                 "CGO_CXXFLAGS", "CGO_LDFLAGS", "SDKROOT", "MACOSX_DEPLOYMENT_TARGET"):
        env.pop(name, None)
    compiler = shutil.which("cc")
    if compiler is None:
        raise ValueError("native release build requires the host C compiler")
    env.update(CGO_ENABLED="1", CC=compiler, GOTOOLCHAIN="local", GOWORK="off",
               GOFLAGS="", GOAMD64="v1", GOARM64="v8.0")
    info = {"cgo_enabled": True, "build_platform": target,
            "compiler": subprocess.check_output([compiler, "--version"], text=True, env=env, timeout=30).splitlines()[0]}
    if target.startswith("darwin/"):
        env["MACOSX_DEPLOYMENT_TARGET"] = "15.0"
        env["SDKROOT"] = subprocess.check_output(["xcrun", "--sdk", "macosx", "--show-sdk-path"], text=True, env=env, timeout=30).strip()
        info["macos_sdk"] = subprocess.check_output(["xcrun", "--sdk", "macosx", "--show-sdk-version"], text=True, env=env, timeout=30).strip()
        info["deployment_target"] = env["MACOSX_DEPLOYMENT_TARGET"]
    return env, info


def static_guest_environment(env):
    if native_platform().split("/")[0] != "linux":
        raise ValueError("static Linux companions must be built on a native Linux runner")
    result = dict(env, CGO_ENABLED="0")
    for name in ("GOOS", "GOARCH", "CC", "CXX", "CGO_CFLAGS", "CGO_CPPFLAGS",
                 "CGO_CXXFLAGS", "CGO_LDFLAGS", "SDKROOT", "MACOSX_DEPLOYMENT_TARGET"):
        result.pop(name, None)
    return result


def inspect_binary(path, target):
    """Check the actual host executable, not just the requested build settings."""
    import re
    settings = subprocess.check_output(["go", "version", "-m", str(path)], text=True, timeout=30)
    goos, arch = target.split("/")
    for setting in ("CGO_ENABLED=1", "GOOS=" + goos, "GOARCH=" + arch):
        if setting not in settings.split():
            raise ValueError("native host binary lacks " + setting)
    if goos == "linux":
        dynamic = subprocess.check_output(["readelf", "-d", str(path)], text=True, timeout=30)
        versions = subprocess.check_output(["readelf", "--version-info", str(path)], text=True, timeout=30)
        required = sorted({tuple(map(int, v.split("."))) for v in re.findall(r"GLIBC_(\d+\.\d+)", versions)})
        if required and required[-1] > (2, 39):
            raise ValueError("binary requires libc newer than Ubuntu 24.04")
        dependencies = re.findall(r"Shared library: \[([^]]+)\]", dynamic)
        if any("/" in dep for dep in dependencies):
            raise ValueError("binary embeds a nonportable library path")
        return {"native_dependencies": dependencies, "glibc_required": ".".join(map(str, required[-1])) if required else None}
    libraries = subprocess.check_output(["otool", "-L", str(path)], text=True, timeout=30)
    dependencies = [line.strip().split(" (", 1)[0] for line in libraries.splitlines()[1:]]
    if any(not name.startswith(("/usr/lib/", "/System/Library/")) for name in dependencies):
        raise ValueError("host executable links a non-system library that is not packaged")
    return {"native_dependencies": dependencies}
