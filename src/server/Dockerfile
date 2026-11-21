FROM    golang:1.26 AS build

WORKDIR /build

COPY    ./shared/stcp.go ./src/server/app.go ./src/server/sessionman.go ./

RUN     CGO_ENABLED=0 GOOS=linux go build -o app app.go sessionman.go stcp.go

FROM    gcr.io/distroless/static:nonroot

WORKDIR /app

COPY    ./shared/cert.crt ./shared/cert.key ./src/server/config.json ./
COPY    --from=build /build/app ./

CMD     ["/app/app"]

EXPOSE  443/tcp
