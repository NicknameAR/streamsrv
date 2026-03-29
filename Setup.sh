#!/bin/bash
# StreamSrv — setup script
# Run: chmod +x setup.sh && ./setup.sh

set -e

echo "==> Installing Go dependencies..."
go get github.com/gorilla/websocket
go get github.com/golang-jwt/jwt/v5
go get github.com/lib/pq
go get golang.org/x/crypto/bcrypt
go get github.com/graphql-go/graphql
go get github.com/graphql-go/handler
go get github.com/redis/go-redis/v9
go mod tidy

echo ""
echo "==> Dependencies installed!"
echo ""
echo "==> Usage:"
echo ""
echo "  Without DB (in-memory only, no accounts):"
echo "    go run server.go graphql.go"
echo ""
echo "  With PostgreSQL:"
echo "    DATABASE_URL='postgres://user:pass@localhost/streamdb?sslmode=disable' go run server.go graphql.go"
echo ""
echo "  With PostgreSQL + Redis + custom JWT secret:"
echo "    DATABASE_URL='...' REDIS_URL='redis://localhost:6379' JWT_SECRET='my-secret' go run server.go graphql.go"
echo ""
echo "  Then open: http://localhost:8080"
echo "  GraphQL:   http://localhost:8080/graphql"