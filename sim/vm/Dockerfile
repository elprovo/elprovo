FROM golang:1.27.2 AS vminit
WORKDIR /src
COPY vminit/ ./
RUN go mod init elprovo-vm-init && CGO_ENABLED=0 go build -trimpath -o /out/elprovo-vm-init .

FROM ubuntu:24.04
RUN apt-get update \
 && DEBIAN_FRONTEND=noninteractive apt-get install --yes --no-install-recommends \
      openssh-server docker.io docker-compose-v2 docker-buildx ca-certificates curl iproute2 iptables iputils-arping jq python3-yaml sudo \
 && rm -rf /var/lib/apt/lists/* /etc/ssh/ssh_host_* /etc/systemd/system/*.wants /etc/systemd/system/*.requires \
 && mkdir -p /var/lib/cloud/seed/nocloud /var/lib/elprovo-vm /etc/systemd/system/multi-user.target.wants
COPY --from=vminit /out/elprovo-vm-init /usr/local/sbin/elprovo-vm-init
COPY units/ /etc/systemd/system/
RUN ln -s /usr/local/sbin/elprovo-vm-init /usr/bin/cloud-init \
 && ln -s /usr/local/sbin/elprovo-vm-init /usr/bin/systemctl \
 && ln -s /usr/local/sbin/elprovo-vm-init /usr/bin/journalctl \
 && ln -s /etc/systemd/system/docker.service /etc/systemd/system/multi-user.target.wants/docker.service \
 && ln -s /etc/systemd/system/ssh.service /etc/systemd/system/multi-user.target.wants/ssh.service \
 && ln -s /etc/systemd/system/elprovo-metadata.service /etc/systemd/system/multi-user.target.wants/elprovo-metadata.service
STOPSIGNAL SIGTERM
ENTRYPOINT ["/usr/local/sbin/elprovo-vm-init"]
