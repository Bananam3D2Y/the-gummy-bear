# Build context is the repo root (see infra/docker-compose.yml), so paths start with brain/ or web/.
FROM golang:1.23 AS build
WORKDIR /src
COPY brain/ .
RUN go mod tidy && CGO_ENABLED=0 go build -o /out/brain ./cmd/brain \
    && CGO_ENABLED=0 go build -o /out/gateway ./cmd/gateway

# distroless/static includes CA certificates, which the HTTPS calls to Gemini/Groq need
FROM gcr.io/distroless/static-debian12 AS brain
COPY --from=build /out/brain /brain
COPY brain/lore /lore
ENV LORE_DIR=/lore
EXPOSE 9101
ENTRYPOINT ["/brain"]

FROM gcr.io/distroless/static-debian12 AS gateway
COPY --from=build /out/gateway /gateway
COPY web /web
ENV WEB_DIR=/web
EXPOSE 8080 9102
ENTRYPOINT ["/gateway"]
