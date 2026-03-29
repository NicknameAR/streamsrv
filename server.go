package main

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	mrand "math/rand"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/gorilla/websocket"
	"github.com/joho/godotenv"
	_ "github.com/lib/pq"
	"github.com/redis/go-redis/v9"
	"golang.org/x/crypto/bcrypt"
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1 << 20,
	WriteBufferSize: 1 << 20,
	CheckOrigin:     func(r *http.Request) bool { return true },
}

type Client struct {
	id        string
	userID    int64
	name      string
	avatarURL string
	room      string
	role      string
	conn      *websocket.Conn
	mu        sync.Mutex
}

func (c *Client) send(v any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	_ = c.conn.WriteJSON(v)
}

type Room struct {
	Name        string
	Password    string
	Clients     map[string]*Client
	History     []ChatPayload
	StreamID    int64
	StartedAt   time.Time
	PeakViewers int
	Title       string
	Category    string
}

type ChatPayload struct {
	ConnID    string `json:"conn_id"`
	Username  string `json:"username"`
	Role      string `json:"role"`
	Text      string `json:"text"`
	AvatarURL string `json:"avatar_url"`
	Ts        int64  `json:"ts"`
}

type WsMsg struct {
	Type string          `json:"type"`
	From string          `json:"from,omitempty"`
	To   string          `json:"to,omitempty"`
	Room string          `json:"room,omitempty"`
	Data json.RawMessage `json:"data,omitempty"`
}

type Claims struct {
	UserID   int64  `json:"user_id"`
	Username string `json:"username"`
	jwt.RegisteredClaims
}

var (
	mu      sync.RWMutex
	clients = make(map[string]*Client)
	rooms   = make(map[string]*Room)
)

var (
	rdbCtx = context.Background()
	rdb    *redis.Client
	db     *sql.DB
	jwtKey []byte
)

func main() {
	if err := godotenv.Load(); err == nil {
		log.Println("[ENV] .env loaded")
	}
	mrand.Seed(time.Now().UnixNano())

	secret := os.Getenv("JWT_SECRET")
	if secret == "" {
		secret = "dev-change-in-prod"
		log.Println("[WARN] JWT_SECRET not set")
	}
	jwtKey = []byte(secret)

	if dsn := os.Getenv("DATABASE_URL"); dsn != "" {
		var err error
		db, err = sql.Open("postgres", dsn)
		if err != nil {
			log.Fatal("[DB]", err)
		}
		db.SetMaxOpenConns(20)
		db.SetMaxIdleConns(5)
		db.SetConnMaxLifetime(5 * time.Minute)
		if err = db.Ping(); err != nil {
			log.Fatalf("[DB] connect failed: %v\nCheck DATABASE_URL in .env", err)
		}
		if err = migrateDB(); err != nil {
			log.Fatal("[DB] migrate:", err)
		}
		log.Println("[DB] PostgreSQL ready")
	} else {
		log.Println("[INFO] No DATABASE_URL — running without persistence")
	}

	if u := os.Getenv("REDIS_URL"); u != "" {
		opt, err := redis.ParseURL(u)
		if err != nil {
			log.Fatal("[Redis]", err)
		}
		rdb = redis.NewClient(opt)
		if err = rdb.Ping(rdbCtx).Err(); err != nil {
			log.Fatal("[Redis]", err)
		}
		log.Println("[Redis] ready")
	}

	os.MkdirAll(filepath.Join("public", "avatars"), 0755)

	mux := http.NewServeMux()
	fs := http.FileServer(http.Dir("./public"))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			http.ServeFile(w, r, "./public/index.html")
			return
		}
		fs.ServeHTTP(w, r)
	})

	mux.HandleFunc("/api/register", apiCORS(handleRegister))
	mux.HandleFunc("/api/login", apiCORS(handleLogin))
	mux.HandleFunc("/api/me", apiCORS(handleMe))
	mux.HandleFunc("/api/avatar/upload", apiCORS(handleAvatarUpload))
	mux.HandleFunc("/api/profile/update", apiCORS(handleProfileUpdate))
	mux.HandleFunc("/api/profile/", apiCORS(handleProfile))
	mux.HandleFunc("/api/streams", apiCORS(handleStreamList))
	mux.HandleFunc("/api/chat/", apiCORS(handleChatHistory))
	mux.HandleFunc("/rooms", apiCORS(handleStreamList))
	mux.HandleFunc("/ws", wsHandler)
	registerGraphQL(mux)

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	log.Printf("[HTTP] http://localhost:%s  |  GraphQL: http://localhost:%s/graphql", port, port)
	srv := &http.Server{Addr: ":" + port, Handler: mux}
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal(err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	log.Println("Shutting down...")
	shutdownWS()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Fatal("Shutdown failed:", err)
	}
	log.Println("Server stopped")
}

func migrateDB() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS users (
			id            BIGSERIAL   PRIMARY KEY,
			username      TEXT        UNIQUE NOT NULL,
			password_hash TEXT        NOT NULL,
			avatar_url    TEXT        NOT NULL DEFAULT '',
			cover_url     TEXT        NOT NULL DEFAULT '',
			bio           TEXT        NOT NULL DEFAULT '',
			accent_color  TEXT        NOT NULL DEFAULT '#7c4dff',
			twitter       TEXT        NOT NULL DEFAULT '',
			youtube       TEXT        NOT NULL DEFAULT '',
			discord       TEXT        NOT NULL DEFAULT '',
			created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		`CREATE TABLE IF NOT EXISTS streams (
			id           BIGSERIAL   PRIMARY KEY,
			user_id      BIGINT      REFERENCES users(id) ON DELETE SET NULL,
			room_name    TEXT        NOT NULL,
			title        TEXT        NOT NULL DEFAULT '',
			category     TEXT        NOT NULL DEFAULT '',
			started_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			ended_at     TIMESTAMPTZ,
			peak_viewers INT         NOT NULL DEFAULT 0,
			duration_sec INT         NOT NULL DEFAULT 0
		)`,
		`CREATE TABLE IF NOT EXISTS chat_messages (
			id        BIGSERIAL   PRIMARY KEY,
			stream_id BIGINT      REFERENCES streams(id) ON DELETE CASCADE,
			user_id   BIGINT,
			username  TEXT        NOT NULL,
			role      TEXT        NOT NULL DEFAULT 'viewer',
			message   TEXT        NOT NULL,
			sent_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		`CREATE TABLE IF NOT EXISTS room_bans (
			id         BIGSERIAL   PRIMARY KEY,
			room_name  TEXT        NOT NULL,
			username   TEXT        NOT NULL,
			banned_by  TEXT        NOT NULL,
			reason     TEXT        NOT NULL DEFAULT '',
			expires_at TIMESTAMPTZ,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			UNIQUE(room_name, username)
		)`,
		`DO $$ BEGIN
			IF NOT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name='users' AND column_name='cover_url') THEN ALTER TABLE users ADD COLUMN cover_url TEXT NOT NULL DEFAULT ''; END IF;
			IF NOT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name='users' AND column_name='accent_color') THEN ALTER TABLE users ADD COLUMN accent_color TEXT NOT NULL DEFAULT '#7c4dff'; END IF;
			IF NOT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name='users' AND column_name='twitter') THEN ALTER TABLE users ADD COLUMN twitter TEXT NOT NULL DEFAULT ''; END IF;
			IF NOT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name='users' AND column_name='youtube') THEN ALTER TABLE users ADD COLUMN youtube TEXT NOT NULL DEFAULT ''; END IF;
			IF NOT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name='users' AND column_name='discord') THEN ALTER TABLE users ADD COLUMN discord TEXT NOT NULL DEFAULT ''; END IF;
		END $$`,
		`CREATE INDEX IF NOT EXISTS idx_streams_room ON streams(room_name)`,
		`CREATE INDEX IF NOT EXISTS idx_streams_user ON streams(user_id)`,
		`CREATE INDEX IF NOT EXISTS idx_streams_live ON streams(ended_at) WHERE ended_at IS NULL`,
		`CREATE INDEX IF NOT EXISTS idx_chat_stream  ON chat_messages(stream_id)`,
		`CREATE INDEX IF NOT EXISTS idx_bans_room    ON room_bans(room_name)`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			short := s
			if len(short) > 60 {
				short = short[:60] + "..."
			}
			return fmt.Errorf("[%s]: %w", short, err)
		}
	}
	log.Println("[DB] Schema OK")
	return nil
}

// ─── JWT ─────────────────────────────────────────────────────────────────────

func makeToken(uid int64, uname string) (string, error) {
	return jwt.NewWithClaims(jwt.SigningMethodHS256, Claims{
		UserID: uid, Username: uname,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(30 * 24 * time.Hour)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
		},
	}).SignedString(jwtKey)
}

func parseToken(s string) (*Claims, bool) {
	t, err := jwt.ParseWithClaims(s, &Claims{}, func(*jwt.Token) (any, error) { return jwtKey, nil })
	if err != nil || !t.Valid {
		return nil, false
	}
	c, ok := t.Claims.(*Claims)
	return c, ok
}

func tokenFromRequest(r *http.Request) (*Claims, bool) {
	if a := r.Header.Get("Authorization"); strings.HasPrefix(a, "Bearer ") {
		return parseToken(strings.TrimPrefix(a, "Bearer "))
	}
	if t := r.URL.Query().Get("token"); t != "" {
		return parseToken(t)
	}
	return nil, false
}

// ─── HTTP helpers ─────────────────────────────────────────────────────────────

func apiCORS(fn http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type,Authorization")
		w.Header().Set("Access-Control-Allow-Methods", "GET,POST,PUT,OPTIONS")
		if r.Method == http.MethodOptions {
			w.WriteHeader(204)
			return
		}
		fn(w, r)
	}
}

func jsonOK(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v) //nolint
}

func jsonFail(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg}) //nolint
}

func requireLogin(w http.ResponseWriter, r *http.Request) (*Claims, bool) {
	c, ok := tokenFromRequest(r)
	if !ok {
		jsonFail(w, "unauthorized", 401)
	}
	return c, ok
}

// ─── Handlers ─────────────────────────────────────────────────────────────────

func handleRegister(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		jsonFail(w, "method not allowed", 405)
		return
	}
	if db == nil {
		jsonFail(w, "Database not configured. Add DATABASE_URL to .env and restart.", 503)
		return
	}
	var b struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if json.NewDecoder(r.Body).Decode(&b) != nil {
		jsonFail(w, "invalid JSON", 400)
		return
	}
	b.Username = strings.TrimSpace(strings.ToLower(b.Username))
	if len(b.Username) < 3 || len(b.Username) > 32 {
		jsonFail(w, "Username: 3–32 characters", 400)
		return
	}
	for _, ch := range b.Username {
		if !((ch >= 'a' && ch <= 'z') || (ch >= '0' && ch <= '9') || ch == '_') {
			jsonFail(w, "Username: lowercase letters, digits, underscore only", 400)
			return
		}
	}
	if len(b.Password) < 6 {
		jsonFail(w, "Password: min 6 characters", 400)
		return
	}
	hash, _ := bcrypt.GenerateFromPassword([]byte(b.Password), bcrypt.DefaultCost)
	var id int64
	err := db.QueryRow(`INSERT INTO users(username,password_hash) VALUES($1,$2) RETURNING id`, b.Username, string(hash)).Scan(&id)
	if err != nil {
		if strings.Contains(err.Error(), "unique") || strings.Contains(err.Error(), "duplicate") {
			jsonFail(w, "Username already taken", 409)
			return
		}
		log.Println("[register]", err)
		jsonFail(w, "internal error", 500)
		return
	}
	token, _ := makeToken(id, b.Username)
	jsonOK(w, map[string]any{"id": id, "username": b.Username, "avatar_url": "", "cover_url": "", "bio": "", "accent_color": "#7c4dff", "token": token})
}

func handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		jsonFail(w, "method not allowed", 405)
		return
	}
	if db == nil {
		jsonFail(w, "Database not configured. Add DATABASE_URL to .env and restart.", 503)
		return
	}
	var b struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if json.NewDecoder(r.Body).Decode(&b) != nil {
		jsonFail(w, "invalid JSON", 400)
		return
	}
	b.Username = strings.TrimSpace(strings.ToLower(b.Username))
	var id int64
	var hash, name, avatarURL, coverURL, bio, accent, twitter, youtube, discord string
	err := db.QueryRow(`SELECT id,username,password_hash,COALESCE(avatar_url,''),COALESCE(cover_url,''),COALESCE(bio,''),COALESCE(accent_color,'#7c4dff'),COALESCE(twitter,''),COALESCE(youtube,''),COALESCE(discord,'') FROM users WHERE username=$1`, b.Username).
		Scan(&id, &name, &hash, &avatarURL, &coverURL, &bio, &accent, &twitter, &youtube, &discord)
	if err == sql.ErrNoRows || bcrypt.CompareHashAndPassword([]byte(hash), []byte(b.Password)) != nil {
		jsonFail(w, "Invalid username or password", 401)
		return
	}
	if err != nil {
		jsonFail(w, "internal error", 500)
		return
	}
	token, _ := makeToken(id, name)
	jsonOK(w, map[string]any{"id": id, "username": name, "avatar_url": avatarURL, "cover_url": coverURL, "bio": bio, "accent_color": accent, "twitter": twitter, "youtube": youtube, "discord": discord, "token": token})
}

func handleMe(w http.ResponseWriter, r *http.Request) {
	claims, ok := requireLogin(w, r)
	if !ok {
		return
	}
	if db == nil {
		jsonOK(w, map[string]any{"id": claims.UserID, "username": claims.Username})
		return
	}
	var avatarURL, coverURL, bio, accent, twitter, youtube, discord string
	var createdAt time.Time
	db.QueryRow(`SELECT COALESCE(avatar_url,''),COALESCE(cover_url,''),COALESCE(bio,''),COALESCE(accent_color,'#7c4dff'),COALESCE(twitter,''),COALESCE(youtube,''),COALESCE(discord,''),created_at FROM users WHERE id=$1`, claims.UserID).
		Scan(&avatarURL, &coverURL, &bio, &accent, &twitter, &youtube, &discord, &createdAt)
	jsonOK(w, map[string]any{"id": claims.UserID, "username": claims.Username, "avatar_url": avatarURL, "cover_url": coverURL, "bio": bio, "accent_color": accent, "twitter": twitter, "youtube": youtube, "discord": discord, "created_at": createdAt})
}

func handleProfileUpdate(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		jsonFail(w, "method not allowed", 405)
		return
	}
	claims, ok := requireLogin(w, r)
	if !ok {
		return
	}
	var b struct {
		Bio, AccentColor, Twitter, YouTube, Discord string
	}
	json.NewDecoder(r.Body).Decode(&b)
	if db != nil {
		db.Exec(`UPDATE users SET bio=$1,accent_color=$2,twitter=$3,youtube=$4,discord=$5 WHERE id=$6`, b.Bio, b.AccentColor, b.Twitter, b.YouTube, b.Discord, claims.UserID)
	}
	jsonOK(w, map[string]any{"ok": true})
}

func handleAvatarUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		jsonFail(w, "method not allowed", 405)
		return
	}
	claims, ok := requireLogin(w, r)
	if !ok {
		return
	}
	field := r.URL.Query().Get("field")
	if field == "" {
		field = "avatar"
	}
	var imgData []byte
	var ext string
	if strings.Contains(r.Header.Get("Content-Type"), "multipart/form-data") {
		r.ParseMultipartForm(8 << 20)
		file, header, err := r.FormFile("file")
		if err != nil {
			jsonFail(w, "missing field 'file'", 400)
			return
		}
		defer file.Close()
		imgData, _ = io.ReadAll(io.LimitReader(file, 8<<20))
		ext = strings.ToLower(filepath.Ext(header.Filename))
	} else {
		var b struct {
			Data string `json:"data"`
		}
		json.NewDecoder(r.Body).Decode(&b)
		if !strings.HasPrefix(b.Data, "data:image/") {
			jsonFail(w, "expected data URL", 400)
			return
		}
		parts := strings.SplitN(b.Data, ",", 2)
		if len(parts) != 2 {
			jsonFail(w, "invalid data URL", 400)
			return
		}
		mime := strings.TrimPrefix(strings.Split(parts[0], ";")[0], "data:image/")
		switch mime {
		case "png":
			ext = ".png"
		case "gif":
			ext = ".gif"
		case "webp":
			ext = ".webp"
		default:
			ext = ".jpg"
		}
		imgData, _ = base64.StdEncoding.DecodeString(parts[1])
	}
	if len(imgData) == 0 {
		jsonFail(w, "empty image", 400)
		return
	}
	if ext == "" {
		ext = ".jpg"
	}
	filename := fmt.Sprintf("%d_%s_%d%s", claims.UserID, field, time.Now().UnixMilli(), ext)
	if err := os.WriteFile(filepath.Join("public", "avatars", filename), imgData, 0644); err != nil {
		jsonFail(w, "failed to save", 500)
		return
	}
	url := "/avatars/" + filename
	if db != nil {
		col := "avatar_url"
		if field == "cover" {
			col = "cover_url"
		}
		db.Exec(`UPDATE users SET `+col+`=$1 WHERE id=$2`, url, claims.UserID)
	}
	jsonOK(w, map[string]any{"url": url})
}

func handleProfile(w http.ResponseWriter, r *http.Request) {
	if db == nil {
		jsonFail(w, "database not configured", 503)
		return
	}
	username := strings.TrimPrefix(r.URL.Path, "/api/profile/")
	username = strings.Split(username, "/")[0]
	if username == "" || username == "update" {
		return
	}
	var id int64
	var avatarURL, coverURL, bio, accent, twitter, youtube, discord string
	var createdAt time.Time
	err := db.QueryRow(`SELECT id,COALESCE(avatar_url,''),COALESCE(cover_url,''),COALESCE(bio,''),COALESCE(accent_color,'#7c4dff'),COALESCE(twitter,''),COALESCE(youtube,''),COALESCE(discord,''),created_at FROM users WHERE username=$1`, username).
		Scan(&id, &avatarURL, &coverURL, &bio, &accent, &twitter, &youtube, &discord, &createdAt)
	if err == sql.ErrNoRows {
		jsonFail(w, "user not found", 404)
		return
	}
	if err != nil {
		jsonFail(w, "internal error", 500)
		return
	}
	type SR struct {
		ID          int64      `json:"id"`
		Title       string     `json:"title"`
		Category    string     `json:"category"`
		StartedAt   time.Time  `json:"started_at"`
		EndedAt     *time.Time `json:"ended_at,omitempty"`
		PeakViewers int        `json:"peak_viewers"`
		DurationSec int        `json:"duration_sec"`
	}
	var streams []SR
	rows, _ := db.Query(`SELECT id,title,category,started_at,ended_at,peak_viewers,duration_sec FROM streams WHERE user_id=$1 ORDER BY started_at DESC LIMIT 20`, id)
	if rows != nil {
		defer rows.Close()
		for rows.Next() {
			var s SR
			rows.Scan(&s.ID, &s.Title, &s.Category, &s.StartedAt, &s.EndedAt, &s.PeakViewers, &s.DurationSec)
			streams = append(streams, s)
		}
	}
	if streams == nil {
		streams = []SR{}
	}
	jsonOK(w, map[string]any{"username": username, "avatar_url": avatarURL, "cover_url": coverURL, "bio": bio, "accent_color": accent, "twitter": twitter, "youtube": youtube, "discord": discord, "created_at": createdAt, "streams": streams})
}

func handleStreamList(w http.ResponseWriter, r *http.Request) {
	mu.RLock()
	list := make([]map[string]any, 0)
	for _, rm := range rooms {
		vc, sn, av := 0, "", ""
		for _, c := range rm.Clients {
			if c.role == "viewer" {
				vc++
			}
			if c.role == "streamer" {
				sn = c.name
				av = c.avatarURL
			}
		}
		list = append(list, map[string]any{"name": rm.Name, "locked": rm.Password != "", "viewers": vc, "streamer": sn, "title": rm.Title, "category": rm.Category, "avatar_url": av, "started_at": rm.StartedAt})
	}
	mu.RUnlock()
	jsonOK(w, list)
}

func handleChatHistory(w http.ResponseWriter, r *http.Request) {
	if db == nil {
		jsonOK(w, []any{})
		return
	}
	roomName := strings.TrimPrefix(r.URL.Path, "/api/chat/")
	var sid int64
	if db.QueryRow(`SELECT id FROM streams WHERE room_name=$1 ORDER BY started_at DESC LIMIT 1`, roomName).Scan(&sid) == sql.ErrNoRows {
		jsonOK(w, []any{})
		return
	}
	type Row struct {
		U, R, M string
		T       time.Time
	}
	rows, err := db.Query(`SELECT username,role,message,sent_at FROM chat_messages WHERE stream_id=$1 ORDER BY sent_at ASC LIMIT 200`, sid)
	if err != nil {
		jsonOK(w, []any{})
		return
	}
	defer rows.Close()
	var msgs []Row
	for rows.Next() {
		var m Row
		rows.Scan(&m.U, &m.R, &m.M, &m.T)
		msgs = append(msgs, m)
	}
	if msgs == nil {
		msgs = []Row{}
	}
	jsonOK(w, msgs)
}

// ─── WebSocket ────────────────────────────────────────────────────────────────

func wsHandler(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}

	var authed *Claims
	if db != nil {
		authed, _ = tokenFromRequest(r)
	}

	role := r.URL.Query().Get("role")
	if role != "streamer" {
		role = "viewer"
	}

	var roomName string
	if role == "streamer" {
		if authed == nil {
			conn.WriteJSON(WsMsg{Type: "error", Data: mustJSON("Login required to stream")})
			conn.Close()
			return
		}
		roomName = authed.Username
	} else {
		roomName = strings.TrimSpace(r.URL.Query().Get("room"))
		if roomName == "" {
			roomName = "lobby"
		}
	}

	roomPass := r.URL.Query().Get("pass")
	streamTitle := r.URL.Query().Get("title")
	streamCat := r.URL.Query().Get("category")

	room := getOrCreateRoom(roomName, roomPass)
	if room == nil {
		conn.WriteJSON(WsMsg{Type: "error", Data: mustJSON("Wrong room password")})
		conn.Close()
		return
	}

	if role == "streamer" {
		mu.RLock()
		for _, c := range clients {
			if c.room == roomName && c.role == "streamer" {
				mu.RUnlock()
				conn.WriteJSON(WsMsg{Type: "error", Data: mustJSON("Channel already has a streamer")})
				conn.Close()
				return
			}
		}
		mu.RUnlock()
	}

	displayName := "guest_" + genID()[:5]
	var userID int64
	var avatarURL string
	if authed != nil {
		displayName = authed.Username
		userID = authed.UserID
		if db != nil {
			db.QueryRow(`SELECT COALESCE(avatar_url,'') FROM users WHERE id=$1`, userID).Scan(&avatarURL)
		}
	}

	if isBanned(roomName, displayName) {
		conn.WriteJSON(WsMsg{Type: "error", Data: mustJSON("You are banned from this channel")})
		conn.Close()
		return
	}

	connID := genID()
	cl := &Client{id: connID, userID: userID, name: displayName, avatarURL: avatarURL, room: roomName, role: role, conn: conn}

	mu.Lock()
	room.Clients[connID] = cl
	clients[connID] = cl
	vc := 0
	for _, c := range room.Clients {
		if c.role == "viewer" {
			vc++
		}
	}
	if vc > room.PeakViewers {
		room.PeakViewers = vc
	}
	if role == "streamer" && db != nil && room.StreamID == 0 {
		if streamTitle != "" {
			room.Title = streamTitle
		}
		if streamCat != "" {
			room.Category = streamCat
		}
		var sid int64
		if e := db.QueryRow(`INSERT INTO streams(user_id,room_name,title,category) VALUES($1,$2,$3,$4) RETURNING id`, nullInt64(userID), roomName, room.Title, room.Category).Scan(&sid); e == nil {
			room.StreamID = sid
			room.StartedAt = time.Now()
		}
	}
	mu.Unlock()

	streamerConnID := ""
	mu.RLock()
	for _, c := range clients {
		if c.room == roomName && c.role == "streamer" {
			streamerConnID = c.id
			break
		}
	}
	mu.RUnlock()

	conn.WriteJSON(WsMsg{Type: "init", Data: mustJSON(map[string]any{
		"id": connID, "name": displayName, "avatar_url": avatarURL,
		"role": role, "room": roomName,
		"streamer_conn_id": streamerConnID,
	})})

	mu.RLock()
	hist := make([]ChatPayload, len(room.History))
	copy(hist, room.History)
	mu.RUnlock()
	for _, h := range hist {
		conn.WriteJSON(WsMsg{Type: "chat", Room: roomName, Data: mustJSON(h)})
	}

	broadcastRoom(roomName, connID, WsMsg{
		Type: "join", From: connID, Room: roomName,
		Data: mustJSON(map[string]any{"id": connID, "name": displayName, "role": role, "avatar_url": avatarURL, "ts": time.Now().Unix()}),
	})

	if role == "viewer" && streamerConnID != "" {
		conn.WriteJSON(WsMsg{
			Type: "streamer-info",
			Data: mustJSON(map[string]any{
				"streamer_conn_id": streamerConnID,
				"title":            room.Title,
				"category":         room.Category,
			}),
		})
	}

	for {
		var m WsMsg
		if err := conn.ReadJSON(&m); err != nil {
			break
		}
		m.From = connID
		m.Room = roomName

		switch m.Type {

		case "stream-update":
			if role != "streamer" {
				continue
			}
			var p struct{ Title, Category string }
			if json.Unmarshal(m.Data, &p) == nil {
				mu.Lock()
				room.Title = p.Title
				room.Category = p.Category
				if db != nil && room.StreamID != 0 {
					db.Exec(`UPDATE streams SET title=$1,category=$2 WHERE id=$3`, p.Title, p.Category, room.StreamID)
				}
				mu.Unlock()
				broadcastRoom(roomName, connID, WsMsg{Type: "stream-meta", Room: roomName, Data: mustJSON(map[string]any{"title": p.Title, "category": p.Category})})
			}

		case "kick":
			if role != "streamer" {
				continue
			}
			var p struct {
				TargetID string `json:"target_id"`
				Reason   string `json:"reason"`
			}
			if json.Unmarshal(m.Data, &p) == nil && p.TargetID != "" {
				mu.RLock()
				t := clients[p.TargetID]
				mu.RUnlock()
				if t != nil {
					t.send(WsMsg{Type: "kicked", Data: mustJSON(map[string]string{"reason": p.Reason})})
					t.conn.Close()
				}
			}

		case "ban":
			if role != "streamer" {
				continue
			}
			var p struct {
				TargetID string `json:"target_id"`
				Reason   string `json:"reason"`
			}
			if json.Unmarshal(m.Data, &p) == nil && p.TargetID != "" {
				mu.RLock()
				t := clients[p.TargetID]
				mu.RUnlock()
				if t != nil {
					if db != nil {
						db.Exec(`INSERT INTO room_bans(room_name,username,banned_by,reason) VALUES($1,$2,$3,$4) ON CONFLICT(room_name,username) DO UPDATE SET reason=EXCLUDED.reason`, roomName, t.name, displayName, p.Reason)
					}
					t.send(WsMsg{Type: "banned", Data: mustJSON(map[string]string{"reason": p.Reason})})
					t.conn.Close()
				}
			}

		case "chat":
			var p struct {
				Text string `json:"text"`
			}
			if err := json.Unmarshal(m.Data, &p); err != nil {
				continue
			}
			if strings.TrimSpace(p.Text) == "" {
				continue
			}
			payload := ChatPayload{ConnID: connID, Username: displayName, Role: role, Text: p.Text, AvatarURL: avatarURL, Ts: time.Now().Unix()}
			mu.Lock()
			room.History = append(room.History, payload)
			if len(room.History) > 100 {
				room.History = room.History[len(room.History)-100:]
			}
			sid := room.StreamID
			mu.Unlock()
			if db != nil && sid != 0 {
				go db.Exec(`INSERT INTO chat_messages(stream_id,user_id,username,role,message) VALUES($1,$2,$3,$4,$5)`, sid, nullInt64(userID), displayName, role, p.Text)
			}

			broadcastAll(roomName, WsMsg{Type: "chat", From: connID, Room: roomName, Data: mustJSON(payload)})

		default:
			if m.To != "" {
				sendDirect(m.To, m)
			} else {
				broadcastRoom(roomName, connID, m)
			}
		}
	}

	mu.Lock()
	delete(clients, connID)
	delete(room.Clients, connID)
	sid := room.StreamID
	peak := room.PeakViewers
	startedAt := room.StartedAt
	if role == "streamer" {
		room.StreamID = 0
		room.Title = ""
		room.Category = ""
	}
	mu.Unlock()
	conn.Close()

	broadcastRoom(roomName, connID, WsMsg{
		Type: "leave", From: connID, Room: roomName,
		Data: mustJSON(map[string]any{"id": connID, "name": displayName, "role": role, "ts": time.Now().Unix()}),
	})
	if role == "streamer" {
		broadcastAll(roomName, WsMsg{Type: "stream-ended"})
	}

	if role == "streamer" && db != nil && sid != 0 {
		dur := int(time.Since(startedAt).Seconds())
		db.Exec(`UPDATE streams SET ended_at=NOW(),peak_viewers=$1,duration_sec=$2 WHERE id=$3`, peak, dur, sid)
		log.Printf("[Stream] ended id=%d peak=%d dur=%ds", sid, peak, dur)
	}
	if rdb != nil {
		rdb.Del(rdbCtx, "streams:live")
	}
}

// ─── Room helpers ─────────────────────────────────────────────────────────────

func getOrCreateRoom(name, pass string) *Room {
	mu.Lock()
	defer mu.Unlock()
	r, ok := rooms[name]
	if !ok {
		r = &Room{Name: name, Password: pass, Clients: make(map[string]*Client), History: make([]ChatPayload, 0, 100)}
		rooms[name] = r
		return r
	}
	if r.Password != "" && r.Password != pass {
		return nil
	}
	return r
}

func isBanned(room, username string) bool {
	if db == nil {
		return false
	}
	var exists bool
	db.QueryRow(`SELECT EXISTS(SELECT 1 FROM room_bans WHERE room_name=$1 AND username=$2 AND (expires_at IS NULL OR expires_at > NOW()))`, room, username).Scan(&exists)
	return exists
}

func broadcastRoom(roomName, excludeID string, m WsMsg) {
	mu.RLock()
	var targets []*Client
	for id, c := range clients {
		if c.room == roomName && id != excludeID {
			targets = append(targets, c)
		}
	}
	mu.RUnlock()
	for _, c := range targets {
		c.send(m)
	}
}

func broadcastAll(roomName string, m WsMsg) {
	mu.RLock()
	var targets []*Client
	for _, c := range clients {
		if c.room == roomName {
			targets = append(targets, c)
		}
	}
	mu.RUnlock()
	for _, c := range targets {
		c.send(m)
	}
}

func sendDirect(to string, m WsMsg) {
	mu.RLock()
	dst := clients[to]
	mu.RUnlock()
	if dst != nil {
		dst.send(m)
	}
}

// ─── Utility ──────────────────────────────────────────────────────────────────

const idAlphabet = "abcdefghijklmnopqrstuvwxyz0123456789"

func genID() string {
	b := make([]byte, 10)
	for i := range b {
		b[i] = idAlphabet[mrand.Intn(len(idAlphabet))]
	}
	return string(b)
}

func mustJSON(v any) json.RawMessage { b, _ := json.Marshal(v); return b }
func nullInt64(v int64) any {
	if v == 0 {
		return nil
	}
	return v
}

func shutdownWS() {
	mu.RLock()
	list := make([]*Client, 0, len(clients))
	for _, c := range clients {
		list = append(list, c)
	}
	mu.RUnlock()
	log.Println("[WS] closing all clients...")
	for _, c := range list {
		_ = c.conn.WriteJSON(WsMsg{Type: "server-shutdown", Data: mustJSON("server is shutting down")})
		time.Sleep(50 * time.Millisecond)
		_ = c.conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, "server shutdown"))
		_ = c.conn.Close()
	}
	mu.Lock()
	clients = make(map[string]*Client)
	rooms = make(map[string]*Room)
	mu.Unlock()
}
