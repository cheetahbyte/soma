FROM golang:1.27.4 AS build
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/soma ./cmd/soma

FROM node:22-bookworm-slim
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates \
 && rm -rf /var/lib/apt/lists/* \
 && npm install -g @anthropic-ai/claude-code
COPY --from=build /out/soma /usr/local/bin/soma
ENV SOMA_HOME=/data/soma \
    HOME=/data/home
EXPOSE 7662
CMD ["soma", "serve", "--listen", "[::]:7662"]
