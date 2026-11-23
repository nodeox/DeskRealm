FROM docker.io/library/rust@sha256:87f3b2f93b82995443a1a558c234212dafe79cfdc3af956539610560369ddcd0

RUN apt-get update -qq \
    && apt-get install -y -qq --no-install-recommends \
        libaom-dev libasound2-dev libclang-dev libdbus-1-dev \
        libgstreamer1.0-dev libgstreamer-plugins-base1.0-dev libgtk-3-dev \
        libopus-dev libpam0g-dev libpulse-dev libudev-dev libvpx-dev libwayland-dev \
        libx11-dev libxcb1-dev libxcb-randr0-dev libxcb-shm0-dev libxcb-xfixes0-dev \
        libxdo-dev libxfixes-dev libxi-dev libxinerama-dev libxkbcommon-dev \
        libxrandr-dev libxtst-dev libyuv-dev \
    && rm -rf /var/lib/apt/lists/*

# Debian Bookworm libyuv-dev has headers and a library without a .pc file.
RUN mkdir /deskrealm-pkgconfig \
    && printf '%s\n' \
        'prefix=/usr' \
        'includedir=${prefix}/include' \
        'Name: libyuv' \
        'Description: Debian libyuv development files' \
        'Version: 0.0.0' \
        'Libs: -lyuv' \
        'Cflags: -I${includedir}' >/deskrealm-pkgconfig/libyuv.pc
ENV PKG_CONFIG_PATH=/deskrealm-pkgconfig
