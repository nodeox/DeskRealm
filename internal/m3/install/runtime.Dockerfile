FROM docker.io/library/rust@sha256:ade39a710ef85f3506cffcfec3e8a24ba090badef1dff2fb798f71d878833fea AS trust

FROM scratch AS center
COPY --from=trust /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY deskrealm /usr/local/bin/deskrealm
COPY web/ /opt/deskrealm/web/
ENTRYPOINT ["/usr/local/bin/deskrealm"]
CMD ["serve"]

FROM docker.io/library/debian@sha256:734a72b3d6142a71993b4bc580ca01d8ca687f77a2c422d96a961d3376b179bc AS rendezvous
COPY --from=trust /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY hbbs /usr/local/bin/hbbs
WORKDIR /var/lib/deskrealm-rendezvous
ENTRYPOINT ["/usr/local/bin/hbbs"]
