FROM golang:1.26-trixie

COPY build/quicmetrics /usr/local/bin/
RUN chmod 755 /usr/local/bin/quicmetrics

ENTRYPOINT ["quicmetrics"]