package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/base64"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	qrcode "github.com/skip2/go-qrcode"
	"golang.org/x/crypto/bcrypt"
)

//go:embed templates/*.html static/*
var assets embed.FS

type App struct {
	db     *sql.DB
	secret []byte
}
type Tournament struct {
	ID          int
	Name        string
	GamesTotal  int
	Status      string
	PlayerCount int
}
type Player struct {
	ID   int
	Name string
}
type GameScore struct {
	Submitted bool
	Score     int
}
type Standing struct {
	PlayerID                    int
	PlayerName                  string
	GameScores                  []GameScore
	Total, GamesCompleted, Rank int
	Average                     float64
}
type Session struct{ csrf string }
type PlayerSession struct {
	playerID, tournamentID int
	csrf                   string
}

func main() {
	dbURL := strings.TrimSpace(os.Getenv("DATABASE_URL"))
	secret := strings.TrimSpace(os.Getenv("SESSION_SECRET"))
	if dbURL == "" {
		log.Fatal("DATABASE_URL is required (use a Neon PostgreSQL connection string)")
	}
	if len(secret) < 32 {
		log.Fatal("SESSION_SECRET must be at least 32 characters")
	}
	db, err := sql.Open("pgx", dbURL)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(6)
	db.SetMaxIdleConns(2)
	db.SetConnMaxLifetime(5 * time.Minute)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err = db.PingContext(ctx); err != nil {
		log.Fatalf("database connection failed: %v", err)
	}
	if err = initDB(ctx, db); err != nil {
		log.Fatalf("database initialization failed: %v", err)
	}
	a := &App{db: db, secret: []byte(secret)}
	mux := http.NewServeMux()
	mux.HandleFunc("/", a.home)
	mux.HandleFunc("/health", a.health)
	mux.HandleFunc("/static/", a.static)
	mux.HandleFunc("/setup", a.setup)
	mux.HandleFunc("/admin/login", a.adminLogin)
	mux.HandleFunc("/admin/logout", a.adminLogout)
	mux.HandleFunc("/admin", a.admin)
	mux.HandleFunc("/admin/tournament/create", a.createTournament)
	mux.HandleFunc("/admin/tournament/start", a.startTournament)
	mux.HandleFunc("/admin/tournament/reset", a.resetTournament)
	mux.HandleFunc("/admin/correct-score", a.correctScore)
	mux.HandleFunc("/admin/export.csv", a.exportCSV)
	mux.HandleFunc("/admin/qr.png", a.qrCode)
	mux.HandleFunc("/play", a.play)
	mux.HandleFunc("/play/login", a.playerLogin)
	mux.HandleFunc("/play/score", a.submitScore)
	mux.HandleFunc("/play/logout", a.playerLogout)
	mux.HandleFunc("/leaderboard", a.leaderboard)
	mux.HandleFunc("/api/leaderboard", a.leaderboardAPI)
	port := os.Getenv("PORT")
	if port == "" {
		port = "10000"
	}
	srv := &http.Server{Addr: "0.0.0.0:" + port, Handler: securityHeaders(mux), ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 20 * time.Second, IdleTimeout: 60 * time.Second}
	log.Printf("Euchre Tournament listening on %s", srv.Addr)
	log.Fatal(srv.ListenAndServe())
}

func initDB(ctx context.Context, db *sql.DB) error {
	qs := []string{
		`CREATE TABLE IF NOT EXISTS app_settings(id INTEGER PRIMARY KEY CHECK(id=1),admin_username TEXT NOT NULL UNIQUE,admin_password_hash TEXT NOT NULL,created_at TIMESTAMPTZ NOT NULL DEFAULT NOW())`,
		`CREATE TABLE IF NOT EXISTS tournaments(id INTEGER PRIMARY KEY,name TEXT NOT NULL,games_total INTEGER NOT NULL CHECK(games_total BETWEEN 1 AND 24),status TEXT NOT NULL CHECK(status IN ('setup','active','completed')),created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),started_at TIMESTAMPTZ,completed_at TIMESTAMPTZ)`,
		`CREATE TABLE IF NOT EXISTS players(id SERIAL PRIMARY KEY,tournament_id INTEGER NOT NULL REFERENCES tournaments(id) ON DELETE CASCADE,name TEXT NOT NULL,pin_hash TEXT NOT NULL,setup_pin TEXT,created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),UNIQUE(tournament_id,name))`,
		`CREATE TABLE IF NOT EXISTS scores(id SERIAL PRIMARY KEY,tournament_id INTEGER NOT NULL REFERENCES tournaments(id) ON DELETE CASCADE,player_id INTEGER NOT NULL REFERENCES players(id) ON DELETE CASCADE,game_number INTEGER NOT NULL CHECK(game_number BETWEEN 1 AND 24),score INTEGER NOT NULL CHECK(score BETWEEN 1 AND 40),submitted_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),UNIQUE(tournament_id,player_id,game_number))`,
		`CREATE TABLE IF NOT EXISTS score_corrections(id SERIAL PRIMARY KEY,score_id INTEGER NOT NULL REFERENCES scores(id) ON DELETE CASCADE,old_score INTEGER NOT NULL,new_score INTEGER NOT NULL,note TEXT NOT NULL DEFAULT '',corrected_at TIMESTAMPTZ NOT NULL DEFAULT NOW())`,
		`CREATE TABLE IF NOT EXISTS admin_sessions(token_hash TEXT PRIMARY KEY,csrf_token TEXT NOT NULL,expires_at TIMESTAMPTZ NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS player_sessions(token_hash TEXT PRIMARY KEY,player_id INTEGER NOT NULL REFERENCES players(id) ON DELETE CASCADE,tournament_id INTEGER NOT NULL REFERENCES tournaments(id) ON DELETE CASCADE,csrf_token TEXT NOT NULL,expires_at TIMESTAMPTZ NOT NULL)`,
		`CREATE INDEX IF NOT EXISTS idx_scores_game ON scores(tournament_id,game_number)`,
	}
	for _, q := range qs {
		if _, err := db.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	_, _ = db.ExecContext(ctx, `ALTER TABLE players ADD COLUMN IF NOT EXISTS setup_pin TEXT`)
	_, _ = db.ExecContext(ctx, `DELETE FROM admin_sessions WHERE expires_at<NOW()`)
	_, _ = db.ExecContext(ctx, `DELETE FROM player_sessions WHERE expires_at<NOW()`)
	return nil
}
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "same-origin")
		w.Header().Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}
func (a *App) static(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/")
	b, err := assets.ReadFile(name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if strings.HasSuffix(name, ".css") {
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
	}
	w.Write(b)
}
func (a *App) home(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	if !a.hasAdmin() {
		http.Redirect(w, r, "/setup", http.StatusSeeOther)
		return
	}
	a.render(w, "index.html", map[string]any{"Title": "Euchre Tournament"})
}
func (a *App) health(w http.ResponseWriter, r *http.Request) {
	ctx, c := context.WithTimeout(r.Context(), 3*time.Second)
	defer c()
	if err := a.db.PingContext(ctx); err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	w.Write([]byte("ok"))
}
func (a *App) setup(w http.ResponseWriter, r *http.Request) {
	if a.hasAdmin() {
		http.Redirect(w, r, "/admin/login", 303)
		return
	}
	if r.Method == http.MethodGet {
		a.render(w, "setup.html", map[string]any{"Title": "First Setup", "CSRF": a.newToken(18)})
		return
	}
	if r.Method != http.MethodPost || r.FormValue("csrf") == "" {
		http.Error(w, "invalid request", 400)
		return
	}
	u := strings.TrimSpace(r.FormValue("username"))
	p := r.FormValue("password")
	if len(u) < 3 || len(u) > 80 || len(p) < 8 || p != r.FormValue("confirm") {
		a.render(w, "setup.html", map[string]any{"Title": "First Setup", "CSRF": a.newToken(18), "Flash": "Username must be 3–80 characters; passwords must be at least 8 characters and match.", "FlashType": "error"})
		return
	}
	h, e := bcrypt.GenerateFromPassword([]byte(p), bcrypt.DefaultCost)
	if e != nil {
		http.Error(w, "could not create account", 500)
		return
	}
	_, e = a.db.ExecContext(r.Context(), `INSERT INTO app_settings(id,admin_username,admin_password_hash) VALUES(1,$1,$2) ON CONFLICT DO NOTHING`, u, string(h))
	if e != nil {
		http.Error(w, "could not create account", 500)
		return
	}
	http.Redirect(w, r, "/admin/login", 303)
}
func (a *App) adminLogin(w http.ResponseWriter, r *http.Request) {
	if !a.hasAdmin() {
		http.Redirect(w, r, "/setup", 303)
		return
	}
	if r.Method == http.MethodGet {
		a.render(w, "login.html", map[string]any{"Title": "Admin Login", "CSRF": a.newToken(18)})
		return
	}
	if r.Method != http.MethodPost || r.FormValue("csrf") == "" {
		http.Error(w, "invalid request", 400)
		return
	}
	var u, h string
	if e := a.db.QueryRowContext(r.Context(), `SELECT admin_username,admin_password_hash FROM app_settings WHERE id=1`).Scan(&u, &h); e != nil {
		http.Error(w, "not configured", 500)
		return
	}
	if u != strings.TrimSpace(r.FormValue("username")) || bcrypt.CompareHashAndPassword([]byte(h), []byte(r.FormValue("password"))) != nil {
		a.render(w, "login.html", map[string]any{"Title": "Admin Login", "CSRF": a.newToken(18), "Flash": "Invalid username or password.", "FlashType": "error"})
		return
	}
	token := a.newToken(32)
	csrf := a.newToken(24)
	_, e := a.db.ExecContext(r.Context(), `INSERT INTO admin_sessions(token_hash,csrf_token,expires_at) VALUES($1,$2,NOW()+INTERVAL '12 hours')`, hashToken(token), csrf)
	if e != nil {
		http.Error(w, "login failed", 500)
		return
	}
	setCookie(w, "admin_session", token, 12*time.Hour)
	http.Redirect(w, r, "/admin", 303)
}
func (a *App) adminLogout(w http.ResponseWriter, r *http.Request) {
	if t, ok := cookieValue(r, "admin_session"); ok {
		_, _ = a.db.ExecContext(r.Context(), `DELETE FROM admin_sessions WHERE token_hash=$1`, hashToken(t))
	}
	clearCookie(w, "admin_session")
	http.Redirect(w, r, "/", 303)
}
func (a *App) admin(w http.ResponseWriter, r *http.Request) {
	s, ok := a.requireAdmin(w, r)
	if !ok {
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", 405)
		return
	}
	t, e := a.currentTournament(r.Context())
	if e != nil && !errors.Is(e, sql.ErrNoRows) {
		http.Error(w, "database error", 500)
		return
	}
	d := map[string]any{"Title": "Admin Dashboard", "Admin": true, "CSRF": s.csrf, "GameOptions": gameNumbers(24)}
	if t != nil {
		ps, _ := a.players(r.Context(), t.ID)
		st, _ := a.standings(r.Context(), t)
		t.PlayerCount = len(ps)
		d["Tournament"] = t
		d["Players"] = ps
		d["Scores"] = st
		d["GameNumbers"] = gameNumbers(t.GamesTotal)
		if t.Status == "setup" {
			rows, _ := a.db.QueryContext(r.Context(), `SELECT name,COALESCE(setup_pin,'') FROM players WHERE tournament_id=1 ORDER BY id`)
			type pin struct{ Name, PIN string }
			var pins []pin
			if rows != nil {
				defer rows.Close()
				for rows.Next() {
					var p pin
					if rows.Scan(&p.Name, &p.PIN) == nil {
						pins = append(pins, p)
					}
				}
			}
			d["PlayerPINs"] = pins
		}
	}
	a.render(w, "admin.html", d)
}
func (a *App) createTournament(w http.ResponseWriter, r *http.Request) {
	s, ok := a.requireAdmin(w, r)
	if !ok {
		return
	}
	if r.Method != http.MethodPost || !a.validCSRF(r, s.csrf) {
		http.Error(w, "invalid request", 400)
		return
	}
	if t, _ := a.currentTournament(r.Context()); t != nil {
		http.Error(w, "a tournament already exists; delete it first", 409)
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	games, _ := strconv.Atoi(r.FormValue("games"))
	if name == "" || len(name) > 120 || games < 1 || games > 24 {
		http.Error(w, "invalid tournament settings", 400)
		return
	}
	seen := map[string]bool{}
	var names []string
	for _, line := range strings.Split(strings.ReplaceAll(r.FormValue("players"), "\r\n", "\n"), "\n") {
		n := strings.TrimSpace(line)
		k := strings.ToLower(n)
		if n != "" && len(n) <= 80 && !seen[k] {
			seen[k] = true
			names = append(names, n)
		}
	}
	if len(names) < 2 {
		http.Error(w, "add at least two unique player names", 400)
		return
	}
	tx, e := a.db.BeginTx(r.Context(), nil)
	if e != nil {
		http.Error(w, "database error", 500)
		return
	}
	defer tx.Rollback()
	_, e = tx.ExecContext(r.Context(), `INSERT INTO tournaments(id,name,games_total,status) VALUES(1,$1,$2,'setup')`, name, games)
	if e != nil {
		http.Error(w, "unable to create tournament", 500)
		return
	}
	for _, n := range names {
		pin := a.pin()
		h, _ := bcrypt.GenerateFromPassword([]byte(pin), bcrypt.DefaultCost)
		if _, e = tx.ExecContext(r.Context(), `INSERT INTO players(tournament_id,name,pin_hash,setup_pin) VALUES(1,$1,$2,$3)`, n, string(h), pin); e != nil {
			http.Error(w, "unable to create players", 500)
			return
		}
	}
	if e = tx.Commit(); e != nil {
		http.Error(w, "database error", 500)
		return
	}
	http.Redirect(w, r, "/admin", 303)
}
func (a *App) startTournament(w http.ResponseWriter, r *http.Request) {
	s, ok := a.requireAdmin(w, r)
	if !ok {
		return
	}
	if r.Method != http.MethodPost || !a.validCSRF(r, s.csrf) {
		http.Error(w, "invalid request", 400)
		return
	}
	t, e := a.currentTournament(r.Context())
	if e != nil || t == nil {
		http.Error(w, "no tournament", 404)
		return
	}
	if t.Status != "setup" {
		http.Error(w, "tournament already started", 409)
		return
	}
	_, e = a.db.ExecContext(r.Context(), `UPDATE tournaments SET status='active',started_at=NOW() WHERE id=1 AND status='setup'`)
	if e == nil {
		_, _ = a.db.ExecContext(r.Context(), `UPDATE players SET setup_pin=NULL WHERE tournament_id=1`)
	}
	if e != nil {
		http.Error(w, "database error", 500)
		return
	}
	http.Redirect(w, r, "/admin", 303)
}
func (a *App) resetTournament(w http.ResponseWriter, r *http.Request) {
	s, ok := a.requireAdmin(w, r)
	if !ok {
		return
	}
	if r.Method != http.MethodPost || !a.validCSRF(r, s.csrf) {
		http.Error(w, "invalid request", 400)
		return
	}
	if _, e := a.db.ExecContext(r.Context(), `DELETE FROM tournaments WHERE id=1`); e != nil {
		http.Error(w, "database error", 500)
		return
	}
	http.Redirect(w, r, "/admin", 303)
}
func (a *App) correctScore(w http.ResponseWriter, r *http.Request) {
	s, ok := a.requireAdmin(w, r)
	if !ok {
		return
	}
	if r.Method != http.MethodPost || !a.validCSRF(r, s.csrf) {
		http.Error(w, "invalid request", 400)
		return
	}
	pid, _ := strconv.Atoi(r.FormValue("player_id"))
	g, _ := strconv.Atoi(r.FormValue("game"))
	score, _ := strconv.Atoi(r.FormValue("score"))
	note := strings.TrimSpace(r.FormValue("note"))
	if pid < 1 || g < 1 || g > 24 || score < 1 || score > 40 || len(note) > 250 {
		http.Error(w, "invalid correction", 400)
		return
	}
	var id, old int
	e := a.db.QueryRowContext(r.Context(), `SELECT id,score FROM scores WHERE tournament_id=1 AND player_id=$1 AND game_number=$2`, pid, g).Scan(&id, &old)
	if e != nil {
		http.Error(w, "that score has not been submitted", 400)
		return
	}
	tx, e := a.db.BeginTx(r.Context(), nil)
	if e != nil {
		http.Error(w, "database error", 500)
		return
	}
	defer tx.Rollback()
	if _, e = tx.ExecContext(r.Context(), `UPDATE scores SET score=$1,updated_at=NOW() WHERE id=$2`, score, id); e != nil {
		http.Error(w, "database error", 500)
		return
	}
	if _, e = tx.ExecContext(r.Context(), `INSERT INTO score_corrections(score_id,old_score,new_score,note) VALUES($1,$2,$3,$4)`, id, old, score, note); e != nil {
		http.Error(w, "database error", 500)
		return
	}
	if e = tx.Commit(); e != nil {
		http.Error(w, "database error", 500)
		return
	}
	http.Redirect(w, r, "/admin", 303)
}
func (a *App) exportCSV(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.requireAdmin(w, r); !ok {
		return
	}
	t, e := a.currentTournament(r.Context())
	if e != nil || t == nil {
		http.Error(w, "no tournament", 404)
		return
	}
	st, e := a.standings(r.Context(), t)
	if e != nil {
		http.Error(w, "database error", 500)
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+safeFilename(t.Name)+`-results.csv"`)
	c := csv.NewWriter(w)
	_ = c.Write([]string{"Tournament", t.Name})
	head := []string{"Player"}
	for i := 1; i <= t.GamesTotal; i++ {
		head = append(head, fmt.Sprintf("Game %d", i))
	}
	head = append(head, "Games Completed", "Total Score", "Average")
	_ = c.Write(head)
	for _, p := range st {
		row := []string{p.PlayerName}
		for _, g := range p.GameScores {
			if g.Submitted {
				row = append(row, strconv.Itoa(g.Score))
			} else {
				row = append(row, "")
			}
		}
		row = append(row, strconv.Itoa(p.GamesCompleted), strconv.Itoa(p.Total), fmt.Sprintf("%.2f", p.Average))
		_ = c.Write(row)
	}
	c.Flush()
}
func (a *App) qrCode(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.requireAdmin(w, r); !ok {
		return
	}
	if t, e := a.currentTournament(r.Context()); e != nil || t == nil {
		http.Error(w, "no tournament", 404)
		return
	}
	url := baseURL(r) + "/play"
	b, e := qrcode.Encode(url, qrcode.Medium, 700)
	if e != nil {
		http.Error(w, "QR generation failed", 500)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Content-Disposition", `attachment; filename="euchre-player-qr.png"`)
	_, _ = w.Write(b)
}
func (a *App) play(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", 405)
		return
	}
	t, e := a.currentTournament(r.Context())
	if e != nil && !errors.Is(e, sql.ErrNoRows) {
		http.Error(w, "database error", 500)
		return
	}
	if token, ok := cookieValue(r, "player_session"); ok {
		ps, e := a.playerSession(r.Context(), token)
		if e == nil {
			a.renderPlay(w, r, t, ps.playerID, ps.csrf)
			return
		}
		clearCookie(w, "player_session")
	}
	var players []Player
	if t != nil {
		players, _ = a.players(r.Context(), t.ID)
	}
	a.render(w, "play_login.html", map[string]any{"Title": "Player Login", "Tournament": t, "Players": players, "CSRF": a.newToken(18)})
}
func (a *App) playerLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.FormValue("csrf") == "" {
		http.Error(w, "invalid request", 400)
		return
	}
	t, e := a.currentTournament(r.Context())
	if e != nil || t == nil {
		http.Error(w, "no tournament", 404)
		return
	}
	if t.Status == "setup" {
		http.Error(w, "tournament has not started", 400)
		return
	}
	pid, _ := strconv.Atoi(r.FormValue("player_id"))
	var h string
	e = a.db.QueryRowContext(r.Context(), `SELECT pin_hash FROM players WHERE id=$1 AND tournament_id=1`, pid).Scan(&h)
	if e != nil || bcrypt.CompareHashAndPassword([]byte(h), []byte(r.FormValue("pin"))) != nil {
		ps, _ := a.players(r.Context(), t.ID)
		a.render(w, "play_login.html", map[string]any{"Title": "Player Login", "Tournament": t, "Players": ps, "CSRF": a.newToken(18), "Flash": "Incorrect player name or PIN.", "FlashType": "error"})
		return
	}
	token := a.newToken(32)
	csrf := a.newToken(24)
	_, e = a.db.ExecContext(r.Context(), `INSERT INTO player_sessions(token_hash,player_id,tournament_id,csrf_token,expires_at) VALUES($1,$2,1,$3,NOW()+INTERVAL '12 hours')`, hashToken(token), pid, csrf)
	if e != nil {
		http.Error(w, "login failed", 500)
		return
	}
	setCookie(w, "player_session", token, 12*time.Hour)
	http.Redirect(w, r, "/play", 303)
}
func (a *App) submitScore(w http.ResponseWriter, r *http.Request) {
	token, ok := cookieValue(r, "player_session")
	if !ok {
		http.Redirect(w, r, "/play", 303)
		return
	}
	ps, e := a.playerSession(r.Context(), token)
	if e != nil {
		clearCookie(w, "player_session")
		http.Redirect(w, r, "/play", 303)
		return
	}
	if r.Method != http.MethodPost || !a.validCSRF(r, ps.csrf) {
		http.Error(w, "invalid request", 400)
		return
	}
	t, e := a.currentTournament(r.Context())
	if e != nil || t == nil || ps.tournamentID != t.ID || t.Status != "active" {
		http.Error(w, "tournament unavailable", 400)
		return
	}
	g, complete, _, e := a.currentGame(r.Context(), t)
	if e != nil {
		http.Error(w, "database error", 500)
		return
	}
	if complete {
		http.Error(w, "tournament complete", 409)
		return
	}
	score, _ := strconv.Atoi(r.FormValue("score"))
	if score < 1 || score > 40 {
		http.Error(w, "score must be 1–40", 400)
		return
	}
	_, e = a.db.ExecContext(r.Context(), `INSERT INTO scores(tournament_id,player_id,game_number,score) VALUES(1,$1,$2,$3) ON CONFLICT(tournament_id,player_id,game_number) DO NOTHING`, ps.playerID, g, score)
	if e != nil {
		http.Error(w, "database error", 500)
		return
	}
	var n int
	_ = a.db.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM scores WHERE tournament_id=1 AND game_number=$1`, g).Scan(&n)
	if n >= t.PlayerCount {
		if g == t.GamesTotal {
			_, _ = a.db.ExecContext(r.Context(), `UPDATE tournaments SET status='completed',completed_at=NOW() WHERE id=1 AND status='active'`)
		}
	}
	http.Redirect(w, r, "/play", 303)
}
func (a *App) playerLogout(w http.ResponseWriter, r *http.Request) {
	if t, ok := cookieValue(r, "player_session"); ok {
		_, _ = a.db.ExecContext(r.Context(), `DELETE FROM player_sessions WHERE token_hash=$1`, hashToken(t))
	}
	clearCookie(w, "player_session")
	http.Redirect(w, r, "/play", 303)
}
func (a *App) leaderboard(w http.ResponseWriter, r *http.Request) {
	t, e := a.currentTournament(r.Context())
	if e != nil && !errors.Is(e, sql.ErrNoRows) {
		http.Error(w, "database error", 500)
		return
	}
	a.render(w, "leaderboard.html", map[string]any{"Title": "Live Leaderboard", "Tournament": t})
}
func (a *App) leaderboardAPI(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	t, e := a.currentTournament(r.Context())
	if errors.Is(e, sql.ErrNoRows) || t == nil {
		_ = json.NewEncoder(w).Encode(map[string]any{"players": []any{}, "completed": false})
		return
	}
	if e != nil {
		http.Error(w, "database error", 500)
		return
	}
	st, e := a.standings(r.Context(), t)
	if e != nil {
		http.Error(w, "database error", 500)
		return
	}
	g, complete, submitted, _ := a.currentGame(r.Context(), t)
	type item struct {
		Name    string  `json:"name"`
		Games   int     `json:"games_completed"`
		Total   int     `json:"total"`
		Average float64 `json:"average"`
		Rank    int     `json:"rank"`
	}
	items := make([]item, 0, len(st))
	for _, p := range st {
		items = append(items, item{p.PlayerName, p.GamesCompleted, p.Total, p.Average, p.Rank})
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"tournament_name": t.Name, "players": items, "completed": complete || t.Status == "completed", "current_game": g, "submitted_current_game": submitted, "player_count": t.PlayerCount})
}
func (a *App) requireAdmin(w http.ResponseWriter, r *http.Request) (Session, bool) {
	token, ok := cookieValue(r, "admin_session")
	if !ok {
		http.Redirect(w, r, "/admin/login", 303)
		return Session{}, false
	}
	var csrf string
	e := a.db.QueryRowContext(r.Context(), `SELECT csrf_token FROM admin_sessions WHERE token_hash=$1 AND expires_at>NOW()`, hashToken(token)).Scan(&csrf)
	if e != nil {
		clearCookie(w, "admin_session")
		http.Redirect(w, r, "/admin/login", 303)
		return Session{}, false
	}
	return Session{csrf}, true
}
func (a *App) playerSession(ctx context.Context, token string) (PlayerSession, error) {
	var p PlayerSession
	e := a.db.QueryRowContext(ctx, `SELECT player_id,tournament_id,csrf_token FROM player_sessions WHERE token_hash=$1 AND expires_at>NOW()`, hashToken(token)).Scan(&p.playerID, &p.tournamentID, &p.csrf)
	return p, e
}
func (a *App) renderPlay(w http.ResponseWriter, r *http.Request, t *Tournament, pid int, csrf string) {
	if t == nil {
		http.Redirect(w, r, "/play", 303)
		return
	}
	var p Player
	e := a.db.QueryRowContext(r.Context(), `SELECT id,name FROM players WHERE id=$1 AND tournament_id=$2`, pid, t.ID).Scan(&p.ID, &p.Name)
	if e != nil {
		clearCookie(w, "player_session")
		http.Redirect(w, r, "/play", 303)
		return
	}
	g, complete, submitted, e := a.currentGame(r.Context(), t)
	if e != nil {
		http.Error(w, "database error", 500)
		return
	}
	already := false
	if !complete {
		var n int
		_ = a.db.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM scores WHERE tournament_id=1 AND player_id=$1 AND game_number=$2`, pid, g).Scan(&n)
		already = n > 0
	}
	a.render(w, "play.html", map[string]any{"Title": "Enter Score", "Tournament": t, "Player": p, "CSRF": csrf, "CurrentGame": g, "Complete": complete || t.Status == "completed", "SubmittedCount": submitted, "AlreadySubmitted": already})
}
func (a *App) currentTournament(ctx context.Context) (*Tournament, error) {
	var t Tournament
	e := a.db.QueryRowContext(ctx, `SELECT id,name,games_total,status,(SELECT COUNT(*) FROM players WHERE tournament_id=t.id) FROM tournaments t WHERE id=1`).Scan(&t.ID, &t.Name, &t.GamesTotal, &t.Status, &t.PlayerCount)
	if e != nil {
		return nil, e
	}
	return &t, nil
}
func (a *App) players(ctx context.Context, tid int) ([]Player, error) {
	rows, e := a.db.QueryContext(ctx, `SELECT id,name FROM players WHERE tournament_id=$1 ORDER BY id`, tid)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	var out []Player
	for rows.Next() {
		var p Player
		if e = rows.Scan(&p.ID, &p.Name); e != nil {
			return nil, e
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
func (a *App) standings(ctx context.Context, t *Tournament) ([]Standing, error) {
	players, e := a.players(ctx, t.ID)
	if e != nil {
		return nil, e
	}
	out := make([]Standing, 0, len(players))
	for _, p := range players {
		s := Standing{PlayerID: p.ID, PlayerName: p.Name, GameScores: make([]GameScore, t.GamesTotal)}
		rows, e := a.db.QueryContext(ctx, `SELECT game_number,score FROM scores WHERE tournament_id=$1 AND player_id=$2`, t.ID, p.ID)
		if e != nil {
			return nil, e
		}
		for rows.Next() {
			var g, v int
			if e = rows.Scan(&g, &v); e != nil {
				rows.Close()
				return nil, e
			}
			if g >= 1 && g <= t.GamesTotal {
				s.GameScores[g-1] = GameScore{true, v}
				s.Total += v
				s.GamesCompleted++
			}
		}
		rows.Close()
		if s.GamesCompleted > 0 {
			s.Average = float64(s.Total) / float64(s.GamesCompleted)
		}
		out = append(out, s)
	}
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			if out[j].Total > out[i].Total || (out[j].Total == out[i].Total && out[j].Average > out[i].Average) || (out[j].Total == out[i].Total && out[j].Average == out[i].Average && strings.ToLower(out[j].PlayerName) < strings.ToLower(out[i].PlayerName)) {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	for i := range out {
		out[i].Rank = i + 1
	}
	return out, nil
}
func (a *App) currentGame(ctx context.Context, t *Tournament) (int, bool, int, error) {
	if t.PlayerCount == 0 {
		return 1, false, 0, nil
	}
	for g := 1; g <= t.GamesTotal; g++ {
		var n int
		e := a.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM scores WHERE tournament_id=$1 AND game_number=$2`, t.ID, g).Scan(&n)
		if e != nil {
			return 0, false, 0, e
		}
		if n < t.PlayerCount {
			return g, false, n, nil
		}
	}
	return t.GamesTotal, true, t.PlayerCount, nil
}
func (a *App) hasAdmin() bool {
	var n int
	e := a.db.QueryRow(`SELECT COUNT(*) FROM app_settings`).Scan(&n)
	return e == nil && n > 0
}
func (a *App) validCSRF(r *http.Request, expected string) bool {
	return expected != "" && r.FormValue("csrf") == expected
}
func (a *App) render(w http.ResponseWriter, name string, data map[string]any) {
	layout, e := assets.ReadFile("templates/layout.html")
	if e != nil {
		http.Error(w, "template error", 500)
		return
	}
	page, e := assets.ReadFile("templates/" + name)
	if e != nil {
		http.Error(w, "template error", 500)
		return
	}
	t, e := template.New("layout").Parse(string(layout) + "\n" + string(page))
	if e != nil {
		http.Error(w, "template error", 500)
		return
	}
	if e = t.ExecuteTemplate(w, "layout", data); e != nil {
		log.Printf("template render: %v", e)
	}
}
func (a *App) newToken(n int) string {
	b := make([]byte, n)
	if _, e := rand.Read(b); e != nil {
		return fmt.Sprintf("fallback-%d", time.Now().UnixNano())
	}
	return base64.RawURLEncoding.EncodeToString(b)
}
func (a *App) pin() string {
	b := make([]byte, 4)
	if _, e := rand.Read(b); e != nil {
		return fmt.Sprintf("%04d", time.Now().UnixNano()%10000)
	}
	return fmt.Sprintf("%04d", (int(b[0])<<24|int(b[1])<<16|int(b[2])<<8|int(b[3]))%10000)
}
func hashToken(s string) string {
	h := sha256.Sum256([]byte(s))
	return base64.RawURLEncoding.EncodeToString(h[:])
}
func cookieValue(r *http.Request, n string) (string, bool) {
	c, e := r.Cookie(n)
	return func() string {
		if e != nil {
			return ""
		}
		return c.Value
	}(), e == nil && c.Value != ""
}
func setCookie(w http.ResponseWriter, n, v string, d time.Duration) {
	http.SetCookie(w, &http.Cookie{Name: n, Value: v, Path: "/", HttpOnly: true, Secure: os.Getenv("RENDER_EXTERNAL_URL") != "", SameSite: http.SameSiteLaxMode, MaxAge: int(d.Seconds())})
}
func clearCookie(w http.ResponseWriter, n string) {
	http.SetCookie(w, &http.Cookie{Name: n, Value: "", Path: "/", HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode, MaxAge: -1})
}
func gameNumbers(n int) []int {
	o := make([]int, n)
	for i := range o {
		o[i] = i + 1
	}
	return o
}
func safeFilename(s string) string {
	var b strings.Builder
	for _, r := range strings.TrimSpace(s) {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			b.WriteRune(r)
		} else if r == ' ' {
			b.WriteRune('_')
		}
	}
	if b.Len() == 0 {
		return "euchre-tournament"
	}
	return b.String()
}
func baseURL(r *http.Request) string {
	scheme := r.Header.Get("X-Forwarded-Proto")
	if scheme != "https" {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}
