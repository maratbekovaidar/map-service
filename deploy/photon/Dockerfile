# Photon has no official Docker image, so we wrap the release jar.
# Photon 1.3.0: Java 21+ required; the search backend is OpenSearch 3.x,
# embedded in the jar (no separate OpenSearch container needed for one city).
FROM eclipse-temurin:21.0.12_8-jre-noble

ARG PHOTON_VERSION=1.3.0
ARG PHOTON_SHA256=a89707c0045e4807b2a1180e132e68e108d998709f48b6c94b98a6e281f571a5

RUN apt-get update \
 && apt-get install -y --no-install-recommends curl ca-certificates \
 && rm -rf /var/lib/apt/lists/* \
 && mkdir -p /opt/photon \
 && curl -fsSL -o /opt/photon/photon.jar \
      "https://github.com/komoot/photon/releases/download/${PHOTON_VERSION}/photon-${PHOTON_VERSION}.jar" \
 && echo "${PHOTON_SHA256}  /opt/photon/photon.jar" | sha256sum -c -

# Bind-mounted data dirs are created by the updater with mode 0777, so a
# fixed unprivileged uid works on any host.
RUN useradd --system --uid 10001 --no-create-home photon \
 && mkdir -p /photon && chown photon /photon
USER photon
WORKDIR /photon
EXPOSE 2322

ENTRYPOINT ["java", "-jar", "/opt/photon/photon.jar"]
CMD ["serve", "-data-dir", "/photon", "-listen-ip", "0.0.0.0"]
