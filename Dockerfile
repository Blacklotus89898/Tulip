# Static Go binary on distroless (the Petal image pattern), but NON-ROOT:
# a money-touching service gets no root even inside its container. The
# deployment sets fsGroup so the journal PVC stays writable.
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/tulip .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/tulip /tulip
EXPOSE 8080
VOLUME ["/data"]
USER nonroot:nonroot
ENTRYPOINT ["/tulip"]
CMD ["-db", "/data/tulip.db"]
