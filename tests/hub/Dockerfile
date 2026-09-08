FROM ubuntu:24.04

# Package installation occurs inside a disposable image, never on the runner's
# service manager/firewall. APT verifies the signed distribution package chain.
RUN apt-get update && apt-get install -y --no-install-recommends \
    nftables iproute2 systemd python3 util-linux passwd ca-certificates \
    && useradd --uid 1001 --user-group --no-create-home --shell /usr/sbin/nologin hub-fixture \
    && rm -rf /var/lib/apt/lists/*
