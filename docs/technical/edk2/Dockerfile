# syntax=docker/dockerfile:1
# Builds the aarch64 EDK2/OVMF firmware (code + vars pflash images) for QEMU,
# in both RELEASE and DEBUG flavours.
#   docker buildx build --progress=plain --target artifacts --output out .
FROM debian:trixie AS build

# Any branch or tag of tianocore/edk2; pinned so the build is reproducible.
#ARG EDK2_REF=edk2-stable202608
ARG EDK2_REF=edk2-stable202411
# EDK2 build targets to produce; RELEASE is quiet, DEBUG logs to the serial console.
ARG BUILD_TARGETS="RELEASE DEBUG"

RUN <<'EOF'
#!/usr/bin/env bash
set -eo pipefail
# One layer: an apt-get update in a layer of its own goes stale and breaks the install.
apt-get update
packages=(
    git ca-certificates
    build-essential python3 python-is-python3 uuid-dev bc
    nasm iasl
    gcc-aarch64-linux-gnu
)
apt-get install -y --no-install-recommends "${packages[@]}"
rm -rf /var/lib/apt/lists/*
EOF

WORKDIR /edk2

RUN <<'EOF'
#!/usr/bin/env bash
set -eo pipefail
git clone --depth 1 --branch "${EDK2_REF}" https://github.com/tianocore/edk2.git .
git submodule update --init --depth 1
EOF

# BaseTools are host C/Python tools that have to be compiled; edksetup.sh only
# points the environment at them, it does not build them.
RUN <<'EOF'
#!/usr/bin/env bash
set -eo pipefail
make -j"$(nproc)" -C BaseTools
EOF

ENV PYTHON_COMMAND=python3
# The toolchain tag is GCC; GCC5 lost its AARCH64 entries in edk2-stable202605.
ENV GCC_AARCH64_PREFIX=aarch64-linux-gnu-

RUN <<'EOF'
#!/usr/bin/env bash
set -eo pipefail
source ./edksetup.sh
flags=(
    -D NETWORK_HTTP_BOOT_ENABLE=TRUE
    -D NETWORK_IP6_ENABLE=TRUE
    -D NETWORK_TLS_ENABLE=TRUE
    -D NETWORK_ISCSI_ENABLE=TRUE
    -D NETWORK_ALLOW_HTTP_CONNECTIONS=TRUE
    -D TPM2_ENABLE=TRUE
    -D TPM2_CONFIG_ENABLE=TRUE
    -D CAVIUM_ERRATUM_27456=TRUE
)
for target in ${BUILD_TARGETS}; do
    build -a AARCH64 -t GCC -b "${target}" -n "$(nproc)" -p ArmVirtPkg/ArmVirtQemu.dsc "${flags[@]}"
    # The output directory was spelled ArmVirtQemu-AARCH64 up to edk2-stable202411
    # and ArmVirtQemu-AArch64 after it, so glob rather than hardcode the case.
    fvdir=$(echo Build/ArmVirtQemu-*/"${target}"_GCC/FV)
    if [[ ! -d "${fvdir}" ]]; then
        echo "FATAL: ${target}: no unique FV directory (got '${fvdir}')" >&2
        exit 1
    fi
    case "${target}" in
        RELEASE) suffix="" ;;
        *)       suffix="-$(echo "${target}" | tr "[:upper:]" "[:lower:]")" ;;
    esac
    mkdir -p /artifacts
    for fd in QEMU_EFI.fd:code QEMU_VARS.fd:vars; do
        src="${fvdir}/${fd%%:*}"
        dst="/artifacts/edk2-aarch64-${fd##*:}${suffix}.fd"
        # truncate creates the file if it is missing, so a GenFds that produced
        # nothing would yield a plausible-looking 64MiB image full of zeros.
        if [[ ! -s "${src}" ]]; then
            echo "FATAL: ${target}: ${src} missing or empty after build" >&2
            exit 1
        fi
        install -m 0644 "${src}" "${dst}"
        # pad CODE/VARS to 64MiB like upstream QEMU's own build does
        truncate -s 64M "${dst}"
    done
done
EOF

FROM scratch AS artifacts
COPY --from=build /artifacts/ /
