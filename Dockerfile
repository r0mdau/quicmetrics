FROM golang:1.19-bullseye

COPY build/quicmetrics /usr/local/bin/
RUN chmod 755 /usr/local/bin/quicmetrics

ENTRYPOINT ["quicmetrics"]