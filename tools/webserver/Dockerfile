FROM busybox
MAINTAINER support@gravwell.io
RUN mkdir /www /tiles
COPY testfiles/index.html /www/
COPY testfiles/leaflet.js /www/
COPY testfiles/leaflet.css /www/
COPY webserver /
COPY docker_config.json /config.json

ENV BIND_ADDRESS=0.0.0.0
ENV BIND_PORT=80
ENV FILE_DIR=/www
ENV TILES_DIR=/tiles
ENV ACCESS_LOG_FILE=/access.log
ENV LOG_FILE=/error.log

CMD ["/webserver", "/config.json"]
