FROM docker.io/library/golang:1.27.3-trixie

RUN apt-get update \
 && apt-get install -y --no-install-recommends sqlite3 curl jq \
 && rm -rf /var/lib/apt/lists/*

# Non-root user matching the usual host UID so bind-mounted files stay yours.
ARG USERNAME=vscode
ARG USER_UID=1000
ARG USER_GID=1000
RUN groupadd --gid ${USER_GID} ${USERNAME} \
 && useradd --uid ${USER_UID} --gid ${USER_GID} -m -s /bin/bash ${USERNAME} \
 && chown -R ${USER_UID}:${USER_GID} /go

USER ${USERNAME}
RUN go install golang.org/x/tools/gopls@v0.24.0 \
 && go install honnef.co/go/tools/cmd/staticcheck@v0.8.2 \
 && go install golang.org/x/vuln/cmd/govulncheck@v1.9.0 \
 && go install github.com/go-delve/delve/cmd/dlv@v1.27.2
