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
	"sort"
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
	ID          int
	Name        string
	Table       int
	Letter      string
	GamesPlayed int
	Assigned    bool
	SitoutOrder int
}
type GameScore struct {
	Submitted bool
	Score     int
}
type MatchPlayerView struct {
	ID           int
	Name         string
	Score        int
	Submitted    bool
	Attested     bool
	Announcement bool
	Ready        bool
	Team         int
	Attesters    []MatchPlayerView
}
type AdminMatchView struct {
	Table             int
	Status            string
	Players           string
	ReadyCount        int
	AnnouncementCount int
	ScoreCount        int
}
type SchedulingAlert struct {
	Table   int
	ACount  int
	BCount  int
	Message string
}
type MatchView struct {
	ID, Table, GameNo               int
	Status                          string
	Partner                         MatchPlayerView
	Opponents                       []MatchPlayerView
	Players                         []MatchPlayerView
	Announced, Ready                bool
	ReadyCount, AnnouncementCount   int
	MyScore                         int
	ScoreSubmitted, MyScoreAttested bool
	ReviewMessage                   string
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
	mux.HandleFunc("/admin/replace-account", a.replaceAdminAccount)
	mux.HandleFunc("/admin/tournament/create", a.createTournament)
	mux.HandleFunc("/admin/tournament/update-games", a.updateTournamentGames)
	mux.HandleFunc("/admin/tournament/start", a.startTournament)
	mux.HandleFunc("/admin/tournament/assignments", a.saveAssignments)
	mux.HandleFunc("/admin/override-position", a.overridePosition)
	mux.HandleFunc("/play/assignment", a.savePlayerAssignment)
	mux.HandleFunc("/play/announcement", a.ackAnnouncement)
	mux.HandleFunc("/play/ready", a.confirmReady)
	mux.HandleFunc("/play/attest", a.attestScore)
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
		`CREATE TABLE IF NOT EXISTS scores(id SERIAL PRIMARY KEY,tournament_id INTEGER NOT NULL REFERENCES tournaments(id) ON DELETE CASCADE,player_id INTEGER NOT NULL REFERENCES players(id) ON DELETE CASCADE,game_number INTEGER NOT NULL CHECK(game_number BETWEEN 1 AND 24),score INTEGER NOT NULL CHECK(score BETWEEN 1 AND 32),submitted_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),UNIQUE(tournament_id,player_id,game_number))`,
		`CREATE TABLE IF NOT EXISTS score_corrections(id SERIAL PRIMARY KEY,score_id INTEGER NOT NULL REFERENCES scores(id) ON DELETE CASCADE,old_score INTEGER NOT NULL,new_score INTEGER NOT NULL,note TEXT NOT NULL DEFAULT '',corrected_at TIMESTAMPTZ NOT NULL DEFAULT NOW())`,
		`CREATE TABLE IF NOT EXISTS admin_sessions(token_hash TEXT PRIMARY KEY,csrf_token TEXT NOT NULL,expires_at TIMESTAMPTZ NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS player_sessions(token_hash TEXT PRIMARY KEY,player_id INTEGER NOT NULL REFERENCES players(id) ON DELETE CASCADE,tournament_id INTEGER NOT NULL REFERENCES tournaments(id) ON DELETE CASCADE,csrf_token TEXT NOT NULL,expires_at TIMESTAMPTZ NOT NULL)`,
		`CREATE INDEX IF NOT EXISTS idx_scores_game ON scores(tournament_id,game_number)`,
		`ALTER TABLE players ADD COLUMN IF NOT EXISTS initial_table INTEGER`,
		`ALTER TABLE players ADD COLUMN IF NOT EXISTS initial_letter TEXT`,
		`ALTER TABLE players ADD COLUMN IF NOT EXISTS current_table INTEGER`,
		`ALTER TABLE players ADD COLUMN IF NOT EXISTS current_letter TEXT`,
		`ALTER TABLE players ADD COLUMN IF NOT EXISTS games_played INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE players ADD COLUMN IF NOT EXISTS sitout_order INTEGER`,
		`ALTER TABLE players ADD COLUMN IF NOT EXISTS position_updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()`,
		`ALTER TABLE scores ADD COLUMN IF NOT EXISTS match_id INTEGER`,
		`ALTER TABLE scores ADD COLUMN IF NOT EXISTS attested_by INTEGER`,
		`ALTER TABLE scores ADD COLUMN IF NOT EXISTS attested_at TIMESTAMPTZ`,
		`ALTER TABLE scores DROP CONSTRAINT IF EXISTS scores_score_check`,
		`ALTER TABLE scores ADD CONSTRAINT scores_score_check CHECK(score BETWEEN 1 AND 32) NOT VALID`,
		`ALTER TABLE tournaments ADD COLUMN IF NOT EXISTS leaderboard_locked BOOLEAN NOT NULL DEFAULT FALSE`,
		`CREATE TABLE IF NOT EXISTS matches(id SERIAL PRIMARY KEY,tournament_id INTEGER NOT NULL REFERENCES tournaments(id) ON DELETE CASCADE,table_number INTEGER NOT NULL,game_number INTEGER NOT NULL DEFAULT 1,status TEXT NOT NULL CHECK(status IN ('waiting','active','review','completed')),a1 INTEGER NOT NULL REFERENCES players(id),b1 INTEGER NOT NULL REFERENCES players(id),a2 INTEGER NOT NULL REFERENCES players(id),b2 INTEGER NOT NULL REFERENCES players(id),winner_team INTEGER,created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),started_at TIMESTAMPTZ,completed_at TIMESTAMPTZ)`,
		`CREATE TABLE IF NOT EXISTS match_readiness(match_id INTEGER NOT NULL REFERENCES matches(id) ON DELETE CASCADE,player_id INTEGER NOT NULL REFERENCES players(id) ON DELETE CASCADE,announcement_ack BOOLEAN NOT NULL DEFAULT FALSE,ready BOOLEAN NOT NULL DEFAULT FALSE,updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),PRIMARY KEY(match_id,player_id))`,
		`CREATE INDEX IF NOT EXISTS idx_matches_table_status ON matches(tournament_id,table_number,status)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_one_pending_match_per_table ON matches(tournament_id,table_number) WHERE status IN ('waiting','active','review')`,
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
		var matchRows []AdminMatchView
		mrs, me := a.db.QueryContext(r.Context(), `SELECT m.table_number,m.status,pa.name||' / '||pb.name||' vs '||pc.name||' / '||pd.name,COALESCE((SELECT COUNT(*) FROM match_readiness mr WHERE mr.match_id=m.id AND mr.ready),0),COALESCE((SELECT COUNT(*) FROM match_readiness mr WHERE mr.match_id=m.id AND mr.announcement_ack),0),COALESCE((SELECT COUNT(*) FROM scores sc WHERE sc.match_id=m.id),0) FROM matches m JOIN players pa ON pa.id=m.a1 JOIN players pb ON pb.id=m.b1 JOIN players pc ON pc.id=m.a2 JOIN players pd ON pd.id=m.b2 WHERE m.tournament_id=$1 AND m.status IN ('waiting','active','review') ORDER BY m.table_number`, t.ID)
		if me == nil {
			for mrs.Next() {
				var mv AdminMatchView
				if mrs.Scan(&mv.Table, &mv.Status, &mv.Players, &mv.ReadyCount, &mv.AnnouncementCount, &mv.ScoreCount) == nil {
					matchRows = append(matchRows, mv)
				}
			}
			mrs.Close()
		}
		d["MatchStatuses"] = matchRows
		maxTables := t.PlayerCount / 4
		alerts, alertErr := a.schedulingAlerts(r.Context(), t.ID, maxTables)
		if alertErr == nil {
			d["SchedulingAlerts"] = alerts
		}
		if t.PlayerCount*t.GamesTotal%4 != 0 {
			d["ImpossibleExactGames"] = true
			d["PlayerGameRemainder"] = (t.PlayerCount * t.GamesTotal) % 4
		}
		d["MaxTables"] = maxTables
		d["TableOptions"] = gameNumbers(maxTables)
		d["SitoutCount"] = t.PlayerCount % 4
		d["GameNumbers"] = gameNumbers(t.GamesTotal)
		feasibleGames := make([]int, 0, 24)
		for g := 1; g <= 24; g++ {
			if t.PlayerCount*g%4 == 0 {
				feasibleGames = append(feasibleGames, g)
			}
		}
		d["FeasibleGameOptions"] = feasibleGames
		if t.Status == "setup" {
			rows, _ := a.db.QueryContext(r.Context(), `SELECT name,COALESCE(setup_pin,'') FROM players WHERE tournament_id=$1 ORDER BY id`, t.ID)
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
func (a *App) replaceAdminAccount(w http.ResponseWriter, r *http.Request) {
	s, ok := a.requireAdmin(w, r)
	if !ok {
		return
	}
	if r.Method != http.MethodPost || !a.validCSRF(r, s.csrf) {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}

	currentPassword := r.FormValue("current_password")
	newUsername := strings.TrimSpace(r.FormValue("new_username"))
	newPassword := r.FormValue("new_password")
	confirmPassword := r.FormValue("confirm_password")
	if len(newUsername) < 3 || len(newUsername) > 80 {
		http.Error(w, "The new username must be between 3 and 80 characters. Return to the admin dashboard and try again.", http.StatusBadRequest)
		return
	}
	if len(newPassword) < 8 || newPassword != confirmPassword {
		http.Error(w, "The new passwords must match and be at least 8 characters long. Return to the admin dashboard and try again.", http.StatusBadRequest)
		return
	}

	var currentHash string
	if err := a.db.QueryRowContext(r.Context(), `SELECT admin_password_hash FROM app_settings WHERE id=1`).Scan(&currentHash); err != nil {
		http.Error(w, "Admin account could not be loaded.", http.StatusInternalServerError)
		return
	}
	if bcrypt.CompareHashAndPassword([]byte(currentHash), []byte(currentPassword)) != nil {
		http.Error(w, "The current password was incorrect. Return to the admin dashboard and try again.", http.StatusUnauthorized)
		return
	}

	newHash, err := bcrypt.GenerateFromPassword([]byte(newPassword), bcrypt.DefaultCost)
	if err != nil {
		http.Error(w, "Could not update the admin account.", http.StatusInternalServerError)
		return
	}

	// Update the one-and-only admin account and revoke every existing admin
	// session in the same transaction. There is never a public registration window.
	tx, err := a.db.BeginTx(r.Context(), nil)
	if err != nil {
		http.Error(w, "Could not update the admin account.", http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(r.Context(), `UPDATE app_settings SET admin_username=$1,admin_password_hash=$2 WHERE id=1`, newUsername, string(newHash)); err != nil {
		http.Error(w, "Could not update the admin account. The new username may already be in use.", http.StatusBadRequest)
		return
	}
	if _, err = tx.ExecContext(r.Context(), `DELETE FROM admin_sessions`); err != nil {
		http.Error(w, "Could not revoke existing admin sessions.", http.StatusInternalServerError)
		return
	}
	if err = tx.Commit(); err != nil {
		http.Error(w, "Could not finish updating the admin account.", http.StatusInternalServerError)
		return
	}
	clearCookie(w, "admin_session")
	http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
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
	if len(names) < 4 {
		http.Error(w, "add at least four unique player names", 400)
		return
	}
	if len(names) > 96 {
		http.Error(w, "this app supports up to 96 players (24 active tables) so 3-digit PINs and table assignments remain manageable", 400)
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
	usedPINs := map[string]bool{}
	for _, n := range names {
		pin := a.pin()
		for usedPINs[pin] {
			pin = a.pin()
		}
		usedPINs[pin] = true
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
func (a *App) updateTournamentGames(w http.ResponseWriter, r *http.Request) {
	s, ok := a.requireAdmin(w, r)
	if !ok {
		return
	}
	if r.Method != http.MethodPost || !a.validCSRF(r, s.csrf) {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	t, err := a.currentTournament(r.Context())
	if err != nil || t == nil || t.Status != "setup" {
		http.Error(w, "the game count can only be changed before the tournament starts", http.StatusConflict)
		return
	}
	games, err := strconv.Atoi(r.FormValue("games"))
	if err != nil || games < 1 || games > 24 {
		http.Error(w, "choose between 1 and 24 games", http.StatusBadRequest)
		return
	}
	if t.PlayerCount*games%4 != 0 {
		http.Error(w, fmt.Sprintf("With %d players, %d games per player creates %d player-game appearances, which cannot be divided into complete four-player games. Choose a game count for which players × games is divisible by four.", t.PlayerCount, games, t.PlayerCount*games), http.StatusBadRequest)
		return
	}
	if _, err = a.db.ExecContext(r.Context(), `UPDATE tournaments SET games_total=$1 WHERE id=$2 AND status='setup'`, games, t.ID); err != nil {
		http.Error(w, "could not update game count", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

func (a *App) saveAssignments(w http.ResponseWriter, r *http.Request) {
	s, ok := a.requireAdmin(w, r)
	if !ok {
		return
	}
	if r.Method != http.MethodPost || !a.validCSRF(r, s.csrf) {
		http.Error(w, "invalid request", 400)
		return
	}
	t, err := a.currentTournament(r.Context())
	if err != nil || t == nil || t.Status != "setup" {
		http.Error(w, "assignments can only be changed before the tournament starts", 409)
		return
	}
	players, err := a.players(r.Context(), t.ID)
	if err != nil {
		http.Error(w, "database error", 500)
		return
	}
	maxTables := len(players) / 4
	type assignment struct {
		id, table, order int
		letter           string
	}
	assignments := make([]assignment, 0, len(players))
	usedOrders := map[string]map[int]bool{"A": {}, "B": {}}
	for _, p := range players {
		table, e1 := strconv.Atoi(r.FormValue(fmt.Sprintf("table_%d", p.ID)))
		letter := strings.ToUpper(strings.TrimSpace(r.FormValue(fmt.Sprintf("letter_%d", p.ID))))
		if e1 != nil || table < 0 || table > maxTables || (letter != "A" && letter != "B") {
			http.Error(w, fmt.Sprintf("Invalid assignment for %s. Table must be 0–%d and letter must be A or B.", p.Name, maxTables), 400)
			return
		}
		order := 0
		if table == 0 {
			order, e1 = strconv.Atoi(r.FormValue(fmt.Sprintf("queue_order_%d", p.ID)))
			if e1 != nil || order < 1 || order > 3 || usedOrders[letter][order] {
				http.Error(w, fmt.Sprintf("Give each Table 0 %s player a unique queue order (1, 2, or 3).", letter), 400)
				return
			}
			usedOrders[letter][order] = true
		}
		assignments = append(assignments, assignment{p.ID, table, order, letter})
	}
	for _, x := range assignments {
		var order any
		if x.table == 0 {
			order = x.order
		}
		if _, err = a.db.ExecContext(r.Context(), `UPDATE players SET initial_table=$1,initial_letter=$2,current_table=$1,current_letter=$2,sitout_order=$3,position_updated_at=NOW() WHERE id=$4 AND tournament_id=$5`, x.table, x.letter, order, x.id, t.ID); err != nil {
			http.Error(w, "could not save assignments", 500)
			return
		}
	}
	http.Redirect(w, r, "/admin", 303)
}
func (a *App) overridePosition(w http.ResponseWriter, r *http.Request) {
	s, ok := a.requireAdmin(w, r)
	if !ok {
		return
	}
	if r.Method != http.MethodPost || !a.validCSRF(r, s.csrf) {
		http.Error(w, "invalid request", 400)
		return
	}
	t, err := a.currentTournament(r.Context())
	if err != nil || t == nil || t.Status != "active" {
		http.Error(w, "position overrides are only available during an active tournament", 409)
		return
	}
	pid, _ := strconv.Atoi(r.FormValue("player_id"))
	table, e := strconv.Atoi(r.FormValue("table"))
	letter := strings.ToUpper(strings.TrimSpace(r.FormValue("letter")))
	maxTables := t.PlayerCount / 4
	if pid < 1 || e != nil || table < 0 || table > maxTables || (letter != "A" && letter != "B") {
		http.Error(w, "invalid player position", 400)
		return
	}
	var locked int
	_ = a.db.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM matches WHERE tournament_id=$1 AND status IN ('active','review') AND $2 IN (a1,b1,a2,b2)`, t.ID, pid).Scan(&locked)
	if locked > 0 {
		http.Error(w, "This player is in an active game or a game under score review. Finish or resolve that game before overriding the position; ongoing games are never interrupted.", 409)
		return
	}
	// Waiting games have not started, so they can safely be rebuilt. Cancel any
	// waiting lineup containing this player and any waiting lineup at the target
	// table; cascading deletes clear every readiness confirmation for those games.
	if _, err = a.db.ExecContext(r.Context(), `DELETE FROM matches WHERE tournament_id=$1 AND status='waiting' AND ($2 IN (a1,b1,a2,b2) OR table_number=$3)`, t.ID, pid, table); err != nil {
		http.Error(w, "could not reset an unstarted lineup", 500)
		return
	}
	var order any = nil
	if table == 0 {
		var next int
		_ = a.db.QueryRowContext(r.Context(), `SELECT COALESCE(MAX(sitout_order),0)+1 FROM players WHERE tournament_id=$1 AND current_table=0 AND current_letter=$2`, t.ID, letter).Scan(&next)
		order = next
	}
	if _, err = a.db.ExecContext(r.Context(), `UPDATE players SET current_table=$1,current_letter=$2,sitout_order=$3,position_updated_at=NOW() WHERE id=$4 AND tournament_id=$5`, table, letter, order, pid, t.ID); err != nil {
		http.Error(w, "could not override position", 500)
		return
	}
	if err = a.createNextMatches(r.Context(), t.ID, maxTables); err != nil {
		http.Error(w, "position saved but table scheduling failed", 500)
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
	t, err := a.currentTournament(r.Context())
	if err != nil || t == nil {
		http.Error(w, "no tournament", 404)
		return
	}
	if t.Status != "setup" {
		http.Error(w, "tournament already started", 409)
		return
	}
	ps, err := a.players(r.Context(), t.ID)
	if err != nil {
		http.Error(w, "database error", 500)
		return
	}
	if len(ps) < 4 {
		http.Error(w, "at least four players are required", 400)
		return
	}
	if len(ps)*t.GamesTotal%4 != 0 {
		http.Error(w, fmt.Sprintf("Cannot start fairly: %d players × %d games = %d player-game appearances, which is not divisible by four. Change the game count before starting so everyone can complete exactly that many games.", len(ps), t.GamesTotal, len(ps)*t.GamesTotal), 400)
		return
	}
	maxTables, extra := len(ps)/4, len(ps)%4
	counts := map[int]map[string]int{}
	zero := map[string]int{"A": 0, "B": 0}
	for _, p := range ps {
		var table sql.NullInt64
		var letter sql.NullString
		if err = a.db.QueryRowContext(r.Context(), `SELECT initial_table,initial_letter FROM players WHERE id=$1`, p.ID).Scan(&table, &letter); err != nil || !table.Valid || !letter.Valid {
			http.Error(w, "Assign every player a starting table and letter, then save assignments.", 400)
			return
		}
		n, l := int(table.Int64), letter.String
		if n < 0 || n > maxTables || (l != "A" && l != "B") {
			http.Error(w, "invalid starting assignment", 400)
			return
		}
		if n == 0 {
			zero[l]++
		} else {
			if counts[n] == nil {
				counts[n] = map[string]int{"A": 0, "B": 0}
			}
			counts[n][l]++
		}
	}
	for n := 1; n <= maxTables; n++ {
		if counts[n] == nil || counts[n]["A"] != 2 || counts[n]["B"] != 2 {
			http.Error(w, fmt.Sprintf("Table %d must have exactly two A players and two B players.", n), 400)
			return
		}
	}
	for _, letter := range []string{"A", "B"} {
		var count int
		if err = a.db.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM players WHERE tournament_id=$1 AND initial_table=0 AND initial_letter=$2`, t.ID, letter).Scan(&count); err != nil {
			http.Error(w, "database error", 500)
			return
		}
		if count > 0 {
			var minOrder, maxOrder, distinct int
			if err = a.db.QueryRowContext(r.Context(), `SELECT MIN(sitout_order),MAX(sitout_order),COUNT(DISTINCT sitout_order) FROM players WHERE tournament_id=$1 AND initial_table=0 AND initial_letter=$2`, t.ID, letter).Scan(&minOrder, &maxOrder, &distinct); err != nil || minOrder != 1 || maxOrder != count || distinct != count {
				http.Error(w, fmt.Sprintf("Table 0 %s players need unique queue orders starting at 1 (for example, 0%s1 then 0%s2).", letter, letter, letter), 400)
				return
			}
		}
	}
	if zero["A"]+zero["B"] != extra {
		http.Error(w, fmt.Sprintf("Table 0 must have exactly %d player(s) because %d player(s) do not fit at active tables.", extra, extra), 400)
		return
	}
	if extra == 0 && (zero["A"] != 0 || zero["B"] != 0) || extra == 1 && !(zero["A"] == 1 && zero["B"] == 0 || zero["A"] == 0 && zero["B"] == 1) || extra == 2 && !(zero["A"] == 1 && zero["B"] == 1) || extra == 3 && !(zero["A"] == 2 && zero["B"] == 1 || zero["A"] == 1 && zero["B"] == 2) {
		http.Error(w, "Table 0 assignments must match the sit-out rules: 0, one letter for 1 extra, one A and one B for 2 extras, or a 2+1 letter split for 3 extras.", 400)
		return
	}
	tx, err := a.db.BeginTx(r.Context(), nil)
	if err != nil {
		http.Error(w, "database error", 500)
		return
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(r.Context(), `UPDATE players SET current_table=initial_table,current_letter=initial_letter,games_played=0,position_updated_at=NOW() WHERE tournament_id=$1`, t.ID); err != nil {
		http.Error(w, "database error", 500)
		return
	}
	_, err = tx.ExecContext(r.Context(), `WITH ordered AS (
		SELECT id, ROW_NUMBER() OVER (PARTITION BY current_letter ORDER BY COALESCE(sitout_order,id),id) AS queue_position
		FROM players WHERE tournament_id=$1 AND current_table=0
	) UPDATE players p SET sitout_order=ordered.queue_position FROM ordered WHERE p.id=ordered.id`, t.ID)
	if err != nil {
		http.Error(w, "database error", 500)
		return
	}
	if _, err = tx.ExecContext(r.Context(), `UPDATE tournaments SET status='active',started_at=NOW(),leaderboard_locked=FALSE WHERE id=$1 AND status='setup'`, t.ID); err != nil {
		http.Error(w, "database error", 500)
		return
	}
	if _, err = tx.ExecContext(r.Context(), `UPDATE players SET setup_pin=NULL WHERE tournament_id=$1`, t.ID); err != nil {
		http.Error(w, "database error", 500)
		return
	}
	if err = tx.Commit(); err != nil {
		http.Error(w, "database error", 500)
		return
	}
	if err = a.createNextMatches(r.Context(), t.ID, maxTables); err != nil {
		http.Error(w, "Could not initialize table games: "+err.Error(), 500)
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
	if pid < 1 || g < 1 || g > 24 || score < 1 || score > 32 || len(note) > 250 {
		http.Error(w, "invalid correction", 400)
		return
	}
	var id, old, matchID int
	e := a.db.QueryRowContext(r.Context(), `SELECT id,score,COALESCE(match_id,0) FROM scores WHERE tournament_id=1 AND player_id=$1 AND game_number=$2`, pid, g).Scan(&id, &old, &matchID)
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
	if matchID > 0 {
		if e = a.evaluateMatch(r.Context(), matchID); e != nil {
			http.Error(w, "score saved but match verification failed", 500)
			return
		}
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
	pid, _ := strconv.Atoi(r.FormValue("player_id"))
	var h string
	e = a.db.QueryRowContext(r.Context(), `SELECT pin_hash FROM players WHERE id=$1 AND tournament_id=1`, pid).Scan(&h)
	if e != nil || (len(r.FormValue("pin")) != 3 || bcrypt.CompareHashAndPassword([]byte(h), []byte(r.FormValue("pin"))) != nil) {
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
	ps, err := a.playerSession(r.Context(), token)
	if err != nil {
		clearCookie(w, "player_session")
		http.Redirect(w, r, "/play", 303)
		return
	}
	if r.Method != http.MethodPost || !a.validCSRF(r, ps.csrf) {
		http.Error(w, "invalid request", 400)
		return
	}
	mid, _ := strconv.Atoi(r.FormValue("match_id"))
	score, e := strconv.Atoi(r.FormValue("score"))
	if mid < 1 || e != nil || score < 1 || score > 32 {
		http.Error(w, "Enter a score from 1 to 32.", 400)
		return
	}
	var status string
	var table int
	var a1, b1, a2, b2 int
	err = a.db.QueryRowContext(r.Context(), `SELECT status,table_number,a1,b1,a2,b2 FROM matches WHERE id=$1 AND tournament_id=$2`, mid, ps.tournamentID).Scan(&status, &table, &a1, &b1, &a2, &b2)
	if err != nil || status != "active" || (ps.playerID != a1 && ps.playerID != b1 && ps.playerID != a2 && ps.playerID != b2) {
		http.Error(w, "This game is not accepting scores for your account.", 409)
		return
	}
	var gameNo int
	err = a.db.QueryRowContext(r.Context(), `SELECT games_played FROM players WHERE id=$1`, ps.playerID).Scan(&gameNo)
	if err != nil || gameNo < 1 {
		http.Error(w, "game number unavailable", 409)
		return
	}
	res, err := a.db.ExecContext(r.Context(), `INSERT INTO scores(tournament_id,player_id,game_number,score,match_id) VALUES($1,$2,$3,$4,$5) ON CONFLICT(tournament_id,player_id,game_number) DO NOTHING`, ps.tournamentID, ps.playerID, gameNo, score, mid)
	if err != nil {
		http.Error(w, "database error", 500)
		return
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		http.Error(w, "Your score has already been submitted for this game.", 409)
		return
	}
	var team string
	if ps.playerID == a1 || ps.playerID == b1 {
		team = "Team 1"
	} else {
		team = "Team 2"
	}
	_ = team
	http.Redirect(w, r, "/play", 303)
}
func (a *App) savePlayerAssignment(w http.ResponseWriter, r *http.Request) {
	token, ok := cookieValue(r, "player_session")
	if !ok {
		http.Redirect(w, r, "/play", http.StatusSeeOther)
		return
	}
	ps, err := a.playerSession(r.Context(), token)
	if err != nil {
		http.Redirect(w, r, "/play", http.StatusSeeOther)
		return
	}
	if r.Method != http.MethodPost || !a.validCSRF(r, ps.csrf) {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	t, err := a.currentTournament(r.Context())
	if err != nil || t.ID != ps.tournamentID || t.Status != "setup" {
		http.Error(w, "starting assignments can only be entered before the tournament starts", http.StatusConflict)
		return
	}
	table, e := strconv.Atoi(r.FormValue("table"))
	letter := strings.ToUpper(strings.TrimSpace(r.FormValue("letter")))
	maxTables := t.PlayerCount / 4
	if e != nil || table < 0 || table > maxTables || (letter != "A" && letter != "B") {
		http.Error(w, fmt.Sprintf("Choose Table 0–%d and letter A or B.", maxTables), http.StatusBadRequest)
		return
	}
	var sitoutOrder any
	if table == 0 {
		order, orderErr := strconv.Atoi(r.FormValue("queue_order"))
		if orderErr != nil || order < 1 || order > 3 {
			http.Error(w, "If assigned to Table 0, enter the queue order the admin gave you (1, 2, or 3).", http.StatusBadRequest)
			return
		}
		var duplicate int
		if err = a.db.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM players WHERE tournament_id=$1 AND current_table=0 AND current_letter=$2 AND sitout_order=$3`, ps.tournamentID, letter, order).Scan(&duplicate); err != nil {
			http.Error(w, "could not check sit-out queue order", http.StatusInternalServerError)
			return
		}
		if duplicate > 0 {
			http.Error(w, "That Table 0 queue order is already assigned to another player with your letter. Ask the admin for the correct order.", http.StatusConflict)
			return
		}
		sitoutOrder = order
	}
	res, err := a.db.ExecContext(r.Context(), `UPDATE players SET initial_table=$1,initial_letter=$2,current_table=$1,current_letter=$2,sitout_order=$3,position_updated_at=NOW() WHERE id=$4 AND tournament_id=$5 AND initial_table IS NULL AND initial_letter IS NULL`, table, letter, sitoutOrder, ps.playerID, ps.tournamentID)
	if err != nil {
		http.Error(w, "could not save starting assignment", http.StatusInternalServerError)
		return
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		http.Error(w, "Your starting assignment is already saved. Ask the admin to correct it if needed.", http.StatusConflict)
		return
	}
	http.Redirect(w, r, "/play", http.StatusSeeOther)
}

func (a *App) ackAnnouncement(w http.ResponseWriter, r *http.Request) { a.updateReadiness(w, r, true) }
func (a *App) confirmReady(w http.ResponseWriter, r *http.Request)    { a.updateReadiness(w, r, false) }
func (a *App) updateReadiness(w http.ResponseWriter, r *http.Request, announcement bool) {
	token, ok := cookieValue(r, "player_session")
	if !ok {
		http.Redirect(w, r, "/play", 303)
		return
	}
	ps, err := a.playerSession(r.Context(), token)
	if err != nil {
		http.Redirect(w, r, "/play", 303)
		return
	}
	if r.Method != http.MethodPost || !a.validCSRF(r, ps.csrf) {
		http.Error(w, "invalid request", 400)
		return
	}
	mid, _ := strconv.Atoi(r.FormValue("match_id"))
	answer := r.FormValue("answer")
	if answer != "yes" && answer != "no" {
		http.Error(w, "choose yes or no", 400)
		return
	}
	var status string
	err = a.db.QueryRowContext(r.Context(), `SELECT status FROM matches WHERE id=$1 AND tournament_id=$2 AND $3 IN (a1,b1,a2,b2)`, mid, ps.tournamentID, ps.playerID).Scan(&status)
	if err != nil || status != "waiting" {
		http.Error(w, "this table is not waiting for readiness", 409)
		return
	}
	if announcement {
		if answer == "yes" {
			_, err = a.db.ExecContext(r.Context(), `UPDATE match_readiness SET announcement_ack=TRUE,updated_at=NOW() WHERE match_id=$1 AND player_id=$2`, mid, ps.playerID)
		} else {
			_, err = a.db.ExecContext(r.Context(), `UPDATE match_readiness SET announcement_ack=FALSE,ready=FALSE,updated_at=NOW() WHERE match_id=$1 AND player_id=$2`, mid, ps.playerID)
		}
	} else {
		if answer == "yes" {
			var ack bool
			_ = a.db.QueryRowContext(r.Context(), `SELECT announcement_ack FROM match_readiness WHERE match_id=$1 AND player_id=$2`, mid, ps.playerID).Scan(&ack)
			if !ack {
				http.Error(w, "Please acknowledge the admin's announcement first.", 400)
				return
			}
			_, err = a.db.ExecContext(r.Context(), `UPDATE match_readiness SET ready=TRUE,updated_at=NOW() WHERE match_id=$1 AND player_id=$2`, mid, ps.playerID)
		} else {
			_, err = a.db.ExecContext(r.Context(), `UPDATE match_readiness SET ready=FALSE,updated_at=NOW() WHERE match_id=$1 AND player_id=$2`, mid, ps.playerID)
		}
	}
	if err != nil {
		http.Error(w, "could not save readiness", 500)
		return
	}
	if announcement && answer == "no" {
		http.Redirect(w, r, "/play?wait=1", 303)
		return
	}
	var n int
	_ = a.db.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM match_readiness WHERE match_id=$1 AND announcement_ack=TRUE AND ready=TRUE`, mid).Scan(&n)
	if n == 4 {
		tx, e := a.db.BeginTx(r.Context(), nil)
		if e == nil {
			var gtotal int
			var tid int
			_ = tx.QueryRowContext(r.Context(), `SELECT t.games_total,m.tournament_id FROM matches m JOIN tournaments t ON t.id=m.tournament_id WHERE m.id=$1`, mid).Scan(&gtotal, &tid)
			var transitioned int64
			var transition sql.Result
			transition, e = tx.ExecContext(r.Context(), `UPDATE matches SET status='active',started_at=NOW() WHERE id=$1 AND status='waiting'`, mid)
			if e == nil {
				transitioned, _ = transition.RowsAffected()
				if transitioned == 0 {
					e = errors.New("match already started")
				}
			}
			if e == nil {
				_, e = tx.ExecContext(r.Context(), `UPDATE players SET games_played=games_played+1 WHERE id IN (SELECT a1 FROM matches WHERE id=$1 UNION SELECT b1 FROM matches WHERE id=$1 UNION SELECT a2 FROM matches WHERE id=$1 UNION SELECT b2 FROM matches WHERE id=$1)`, mid)
			}
			if e == nil {
				_, e = tx.ExecContext(r.Context(), `UPDATE tournaments SET leaderboard_locked=TRUE WHERE id=$1 AND EXISTS(SELECT 1 FROM matches m JOIN players p ON p.id IN (m.a1,m.b1,m.a2,m.b2) WHERE m.id=$2 AND p.games_played >= GREATEST(1,$3))`, tid, mid, gtotal-3)
			}
			if e == nil {
				e = tx.Commit()
			} else {
				tx.Rollback()
			}
		}
	}
	http.Redirect(w, r, "/play", 303)
}
func (a *App) attestScore(w http.ResponseWriter, r *http.Request) {
	token, ok := cookieValue(r, "player_session")
	if !ok {
		http.Redirect(w, r, "/play", 303)
		return
	}
	ps, err := a.playerSession(r.Context(), token)
	if err != nil {
		http.Redirect(w, r, "/play", 303)
		return
	}
	if r.Method != http.MethodPost || !a.validCSRF(r, ps.csrf) {
		http.Error(w, "invalid request", 400)
		return
	}
	mid, _ := strconv.Atoi(r.FormValue("match_id"))
	target, _ := strconv.Atoi(r.FormValue("target_player_id"))
	attester, _ := strconv.Atoi(r.FormValue("attester_player_id"))
	pin := r.FormValue("pin")
	var a1, b1, a2, b2 int
	var status string
	err = a.db.QueryRowContext(r.Context(), `SELECT a1,b1,a2,b2,status FROM matches WHERE id=$1 AND tournament_id=$2`, mid, ps.tournamentID).Scan(&a1, &b1, &a2, &b2, &status)
	if err != nil || status != "active" {
		http.Error(w, "game unavailable for attestation", 409)
		return
	}
	isMember := func(id int) bool { return id == a1 || id == b1 || id == a2 || id == b2 }
	if !isMember(ps.playerID) || !isMember(attester) || !isMember(target) || target == attester {
		http.Error(w, "invalid player for attestation", 400)
		return
	}
	team := func(id int) int {
		if id == a1 || id == b1 {
			return 1
		}
		return 2
	}
	if team(attester) == team(target) {
		http.Error(w, "Only a player from the opposing team can attest this score.", 400)
		return
	}
	var hash string
	if err = a.db.QueryRowContext(r.Context(), `SELECT pin_hash FROM players WHERE id=$1 AND tournament_id=$2`, attester, ps.tournamentID).Scan(&hash); err != nil || bcrypt.CompareHashAndPassword([]byte(hash), []byte(pin)) != nil {
		http.Error(w, "The PIN was incorrect. The selected attester must enter their own tournament PIN.", 401)
		return
	}
	res, err := a.db.ExecContext(r.Context(), `UPDATE scores SET attested_by=$1,attested_at=NOW() WHERE match_id=$2 AND player_id=$3 AND attested_by IS NULL`, attester, mid, target)
	if err != nil {
		http.Error(w, "database error", 500)
		return
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		http.Error(w, "That player's score is missing or has already been attested.", 409)
		return
	}
	if err = a.evaluateMatch(r.Context(), mid); err != nil {
		http.Error(w, "Score verification failed: "+err.Error(), 500)
		return
	}
	http.Redirect(w, r, "/play", 303)
}

func (a *App) createNextMatches(ctx context.Context, tid, maxTables int) error {
	for table := 1; table <= maxTables; table++ {
		var pending int
		if err := a.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM matches WHERE tournament_id=$1 AND table_number=$2 AND status IN ('waiting','active','review')`, tid, table).Scan(&pending); err != nil {
			return err
		}
		if pending > 0 {
			continue
		}

		rows, err := a.db.QueryContext(ctx, `SELECT p.id,p.current_letter FROM players p
			WHERE p.tournament_id=$1 AND p.current_table=$2
			AND p.games_played < (SELECT games_total FROM tournaments WHERE id=$1)
			AND NOT EXISTS(SELECT 1 FROM matches m WHERE m.tournament_id=$1 AND m.status IN ('waiting','active','review') AND p.id IN (m.a1,m.b1,m.a2,m.b2))
			ORDER BY p.position_updated_at,p.id`, tid, table)
		if err != nil {
			return err
		}
		type pl struct {
			id int
			l  string
		}
		var aa, bb []int
		for rows.Next() {
			var p pl
			if err = rows.Scan(&p.id, &p.l); err != nil {
				rows.Close()
				return err
			}
			if p.l == "A" {
				aa = append(aa, p.id)
			} else if p.l == "B" {
				bb = append(bb, p.id)
			}
		}
		if err = rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
		if len(aa) < 2 || len(bb) < 2 {
			continue
		}

		// Winners from the table's most recently completed game stay as anchors
		// for the next lineup when they still have games remaining. Any additional
		// arrivals wait in FIFO order and fill the vacant A/B slots.
		var anchorA, anchorB sql.NullInt64
		_ = a.db.QueryRowContext(ctx, `SELECT
			CASE WHEN winner_team=1 THEN a1 ELSE a2 END,
			CASE WHEN winner_team=1 THEN b1 ELSE b2 END
			FROM matches WHERE tournament_id=$1 AND table_number=$2 AND status='completed'
			ORDER BY id DESC LIMIT 1`, tid, table).Scan(&anchorA, &anchorB)
		prioritize := func(ids []int, anchor sql.NullInt64) []int {
			out := make([]int, 0, len(ids))
			if anchor.Valid {
				for _, id := range ids {
					if id == int(anchor.Int64) {
						out = append(out, id)
						break
					}
				}
			}
			for _, id := range ids {
				found := false
				for _, already := range out {
					if id == already {
						found = true
						break
					}
				}
				if !found {
					out = append(out, id)
				}
			}
			return out
		}
		aa = prioritize(aa, anchorA)
		bb = prioritize(bb, anchorB)
		aa = aa[:2]
		bb = bb[:2]
		a1, b1, a2, b2 := a.choosePairing(ctx, tid, aa, bb)
		var mid int
		err = a.db.QueryRowContext(ctx, `INSERT INTO matches(tournament_id,table_number,a1,b1,a2,b2,status) VALUES($1,$2,$3,$4,$5,$6,'waiting') RETURNING id`, tid, table, a1, b1, a2, b2).Scan(&mid)
		if err != nil {
			return err
		}
		for _, pid := range []int{a1, b1, a2, b2} {
			if _, err = a.db.ExecContext(ctx, `INSERT INTO match_readiness(match_id,player_id) VALUES($1,$2)`, mid, pid); err != nil {
				return err
			}
		}
	}
	return nil
}

// schedulingAlerts identifies active players who still need games but cannot
// currently be formed into a complete A/A/B/B lineup at an idle table. The
// admin can resolve these cases with the position override (including changing
// a player's letter); an ongoing game is never moved or rewritten.
func (a *App) schedulingAlerts(ctx context.Context, tid, maxTables int) ([]SchedulingAlert, error) {
	var alerts []SchedulingAlert
	for table := 1; table <= maxTables; table++ {
		var pending int
		if err := a.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM matches WHERE tournament_id=$1 AND table_number=$2 AND status IN ('waiting','active','review')`, tid, table).Scan(&pending); err != nil {
			return nil, err
		}
		if pending > 0 {
			continue
		}
		var aCount, bCount, total int
		err := a.db.QueryRowContext(ctx, `SELECT
			COUNT(*) FILTER (WHERE p.current_letter='A'),
			COUNT(*) FILTER (WHERE p.current_letter='B'),
			COUNT(*)
			FROM players p JOIN tournaments t ON t.id=p.tournament_id
			WHERE p.tournament_id=$1 AND p.current_table=$2
			AND p.games_played<t.games_total
			AND NOT EXISTS(SELECT 1 FROM matches m WHERE m.tournament_id=$1 AND m.status IN ('waiting','active','review') AND p.id IN (m.a1,m.b1,m.a2,m.b2))`, tid, table).Scan(&aCount, &bCount, &total)
		if err != nil {
			return nil, err
		}
		if total > 0 && (aCount != 2 || bCount != 2) {
			message := fmt.Sprintf("Table %d cannot form a game: %d A player(s), %d B player(s) waiting; exactly two of each are needed. Use Admin Position Override to move a waiting player or change their letter.", table, aCount, bCount)
			alerts = append(alerts, SchedulingAlert{Table: table, ACount: aCount, BCount: bCount, Message: message})
		}
	}
	var unfinished, pending int
	if err := a.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM players p JOIN tournaments t ON t.id=p.tournament_id WHERE p.tournament_id=$1 AND p.games_played<t.games_total`, tid).Scan(&unfinished); err != nil {
		return nil, err
	}
	if err := a.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM matches WHERE tournament_id=$1 AND status IN ('waiting','active','review')`, tid).Scan(&pending); err != nil {
		return nil, err
	}
	if unfinished > 0 && pending == 0 && len(alerts) == 0 {
		alerts = append(alerts, SchedulingAlert{Table: 0, Message: fmt.Sprintf("%d player(s) still need games, but no game is waiting, active, or available to create. Review all current positions and letters, then use Admin Position Override to resolve the lineup.", unfinished)})
	}
	return alerts, nil
}

func (a *App) choosePairing(ctx context.Context, tid int, aa, bb []int) (int, int, int, int) {
	// Prefer a pairing that does not repeat either player's most recent partner.
	repeats := func(x, y int) bool {
		var partner sql.NullInt64
		err := a.db.QueryRowContext(ctx, `SELECT CASE WHEN a1=$2 THEN b1 WHEN b1=$2 THEN a1 WHEN a2=$2 THEN b2 WHEN b2=$2 THEN a2 END FROM matches WHERE tournament_id=$1 AND status='completed' AND $2 IN (a1,b1,a2,b2) ORDER BY id DESC LIMIT 1`, tid, x).Scan(&partner)
		return err == nil && partner.Valid && int(partner.Int64) == y
	}
	if !repeats(aa[0], bb[0]) && !repeats(aa[1], bb[1]) {
		return aa[0], bb[0], aa[1], bb[1]
	}
	if !repeats(aa[0], bb[1]) && !repeats(aa[1], bb[0]) {
		return aa[0], bb[1], aa[1], bb[0]
	}
	return aa[0], bb[1], aa[1], bb[0]
}
func (a *App) evaluateMatch(ctx context.Context, mid int) error {
	var tid, table, a1, b1, a2, b2 int
	var status string
	if err := a.db.QueryRowContext(ctx, `SELECT tournament_id,table_number,a1,b1,a2,b2,status FROM matches WHERE id=$1`, mid).Scan(&tid, &table, &a1, &b1, &a2, &b2, &status); err != nil {
		return err
	}
	if status != "active" && status != "review" {
		return nil
	}
	type sv struct{ id, score, attested int }
	var vals []sv
	rows, err := a.db.QueryContext(ctx, `SELECT player_id,score,COALESCE(attested_by,0) FROM scores WHERE match_id=$1 ORDER BY player_id`, mid)
	if err != nil {
		return err
	}
	for rows.Next() {
		var x sv
		if err = rows.Scan(&x.id, &x.score, &x.attested); err != nil {
			rows.Close()
			return err
		}
		vals = append(vals, x)
	}
	rows.Close()
	if len(vals) < 4 {
		return nil
	}
	for _, v := range vals {
		if v.attested == 0 {
			return nil
		}
	}
	score := map[int]int{}
	for _, v := range vals {
		score[v.id] = v.score
	}
	// User-requested validation: the four scores must form two matching pairs,
	// without requiring the pair-matching check itself to infer team identity.
	pairScores := []int{score[a1], score[b1], score[a2], score[b2]}
	sort.Ints(pairScores)
	if pairScores[0] != pairScores[1] || pairScores[2] != pairScores[3] || pairScores[0] == pairScores[2] {
		_, err = a.db.ExecContext(ctx, `UPDATE matches SET status='review' WHERE id=$1`, mid)
		return err
	}
	// The recorded lineup is still needed to determine the winner. If matching
	// score pairs cross the two actual teams, the team totals can tie; that
	// cannot safely be auto-resolved and is sent to the admin.
	team1Score := score[a1] + score[b1]
	team2Score := score[a2] + score[b2]
	if team1Score == team2Score {
		_, err = a.db.ExecContext(ctx, `UPDATE matches SET status='review' WHERE id=$1`, mid)
		return err
	}
	winner := 1
	if team2Score > team1Score {
		winner = 2
	}
	var maxTables int
	var playerCount int
	_ = a.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM players WHERE tournament_id=$1`, tid).Scan(&playerCount)
	maxTables = playerCount / 4
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE matches SET status='completed',winner_team=$1,completed_at=NOW() WHERE id=$2 AND status IN ('active','review')`, winner, mid)
	if err != nil {
		return err
	}
	changed, _ := res.RowsAffected()
	if changed == 0 {
		return nil
	}
	winners := []int{}
	losers := []int{}
	if winner == 1 {
		winners = []int{a1, b1}
		losers = []int{a2, b2}
	} else {
		winners = []int{a2, b2}
		losers = []int{a1, b1}
	}
	// A losers move up one table; B losers move down one. Endpoint sit-outs exchange with the front of that letter's queue.
	for _, pid := range losers {
		var letter string
		var gamesPlayed, gamesTotal int
		if err = tx.QueryRowContext(ctx, `SELECT p.current_letter,p.games_played,t.games_total FROM players p JOIN tournaments t ON t.id=p.tournament_id WHERE p.id=$1`, pid).Scan(&letter, &gamesPlayed, &gamesTotal); err != nil {
			return err
		}
		finished := gamesPlayed >= gamesTotal
		dest := table
		if letter == "A" {
			dest = table + 1
			if dest > maxTables {
				dest = 1
			}
		} else {
			dest = table - 1
			if dest < 1 {
				dest = maxTables
			}
		}
		endpoint := (letter == "A" && table == maxTables) || (letter == "B" && table == 1)
		if endpoint {
			var waitingID int
			qe := tx.QueryRowContext(ctx, `SELECT id FROM players WHERE tournament_id=$1 AND current_table=0 AND current_letter=$2 ORDER BY sitout_order,id LIMIT 1`, tid, letter).Scan(&waitingID)
			if qe == nil {
				if !finished {
					var nextOrder int
					if err = tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(sitout_order),0)+1 FROM players WHERE tournament_id=$1 AND current_table=0 AND current_letter=$2`, tid, letter).Scan(&nextOrder); err != nil {
						return err
					}
					if _, err = tx.ExecContext(ctx, `UPDATE players SET current_table=0,sitout_order=$1,position_updated_at=NOW() WHERE id=$2`, nextOrder, pid); err != nil {
						return err
					}
				}
				if _, err = tx.ExecContext(ctx, `UPDATE players SET current_table=$1,sitout_order=NULL,position_updated_at=NOW() WHERE id=$2`, dest, waitingID); err != nil {
					return err
				}
				continue
			}
		}
		if !finished {
			if _, err = tx.ExecContext(ctx, `UPDATE players SET current_table=$1,sitout_order=NULL,position_updated_at=NOW() WHERE id=$2`, dest, pid); err != nil {
				return err
			}
		}
	}
	// Winners who still have games remaining stay at this table. Finished players are retired and are never selected again.
	for _, pid := range winners {
		var gamesPlayed, gamesTotal int
		if err = tx.QueryRowContext(ctx, `SELECT p.games_played,t.games_total FROM players p JOIN tournaments t ON t.id=p.tournament_id WHERE p.id=$1`, pid).Scan(&gamesPlayed, &gamesTotal); err != nil {
			return err
		}
		if gamesPlayed < gamesTotal {
			if _, err = tx.ExecContext(ctx, `UPDATE players SET current_table=$1,sitout_order=NULL,position_updated_at=NOW() WHERE id=$2`, table, pid); err != nil {
				return err
			}
		}
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	if err = a.createNextMatches(ctx, tid, maxTables); err != nil {
		return err
	}
	var unfinished int
	_ = a.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM players p JOIN tournaments t ON t.id=p.tournament_id WHERE p.tournament_id=$1 AND p.games_played<t.games_total`, tid).Scan(&unfinished)
	var pending int
	_ = a.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM matches WHERE tournament_id=$1 AND status IN ('waiting','active','review')`, tid).Scan(&pending)
	if unfinished == 0 && pending == 0 {
		_, err = a.db.ExecContext(ctx, `UPDATE tournaments SET status='completed',completed_at=NOW() WHERE id=$1`, tid)
	}
	return err
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
	locked := false
	if t != nil {
		_ = a.db.QueryRowContext(r.Context(), `SELECT leaderboard_locked FROM tournaments WHERE id=$1`, t.ID).Scan(&locked)
	}
	a.render(w, "leaderboard.html", map[string]any{"Title": "Live Leaderboard", "Tournament": t, "Locked": locked})
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
	var locked bool
	_ = a.db.QueryRowContext(r.Context(), `SELECT leaderboard_locked FROM tournaments WHERE id=$1`, t.ID).Scan(&locked)
	if locked {
		_ = json.NewEncoder(w).Encode(map[string]any{"tournament_name": t.Name, "players": []any{}, "locked": true, "completed": t.Status == "completed"})
		return
	}
	st, e := a.standings(r.Context(), t)
	if e != nil {
		http.Error(w, "database error", 500)
		return
	}
	var g, submitted, eligible int
	_ = a.db.QueryRowContext(r.Context(), `SELECT COALESCE(MAX(games_played),0) FROM players WHERE tournament_id=$1`, t.ID).Scan(&g)
	if g < 1 {
		g = 1
	}
	_ = a.db.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM scores WHERE tournament_id=$1 AND game_number=$2`, t.ID, g).Scan(&submitted)
	_ = a.db.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM players WHERE tournament_id=$1 AND games_played >= $2`, t.ID, g).Scan(&eligible)
	complete := t.Status == "completed"
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
	_ = json.NewEncoder(w).Encode(map[string]any{"tournament_name": t.Name, "players": items, "completed": complete || t.Status == "completed", "current_game": g, "submitted_current_game": submitted, "player_count": eligible, "locked": false})
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
		var e error
		t, e = a.currentTournament(r.Context())
		if e != nil || t == nil {
			http.Redirect(w, r, "/play", 303)
			return
		}
	}
	var p Player
	err := a.db.QueryRowContext(r.Context(), `SELECT id,name,COALESCE(current_table,0),COALESCE(current_letter,''),games_played,(initial_table IS NOT NULL AND initial_letter IS NOT NULL) FROM players WHERE id=$1 AND tournament_id=$2`, pid, t.ID).Scan(&p.ID, &p.Name, &p.Table, &p.Letter, &p.GamesPlayed, &p.Assigned)
	if err != nil {
		clearCookie(w, "player_session")
		http.Redirect(w, r, "/play", 303)
		return
	}
	data := map[string]any{"Title": "Player Tournament", "Tournament": t, "Player": p, "CSRF": csrf, "Complete": t.Status == "completed", "PlayerDone": p.GamesPlayed >= t.GamesTotal, "WaitForAnnouncement": r.URL.Query().Get("wait") == "1", "MaxTables": t.PlayerCount / 4, "TableOptions": gameNumbers(t.PlayerCount / 4)}
	if t.Status == "setup" {
		data["SetupOnly"] = true
		a.render(w, "play.html", data)
		return
	}
	var mv MatchView
	err = a.db.QueryRowContext(r.Context(), `SELECT id,table_number,status,a1,b1,a2,b2 FROM matches WHERE tournament_id=$1 AND status IN ('waiting','active','review') AND $2 IN (a1,b1,a2,b2) ORDER BY id DESC LIMIT 1`, t.ID, pid).Scan(&mv.ID, &mv.Table, &mv.Status, new(int), new(int), new(int), new(int))
	// Load the match IDs separately; using a stable row struct avoids changing the fixed lineup after start.
	if err == nil {
		var a1, b1, a2, b2 int
		_ = a.db.QueryRowContext(r.Context(), `SELECT a1,b1,a2,b2 FROM matches WHERE id=$1`, mv.ID).Scan(&a1, &b1, &a2, &b2)
		var players []MatchPlayerView
		for _, id := range []int{a1, b1, a2, b2} {
			var x MatchPlayerView
			x.ID = id
			_ = a.db.QueryRowContext(r.Context(), `SELECT name FROM players WHERE id=$1`, id).Scan(&x.Name)
			_ = a.db.QueryRowContext(r.Context(), `SELECT announcement_ack,ready FROM match_readiness WHERE match_id=$1 AND player_id=$2`, mv.ID, id).Scan(&x.Announcement, &x.Ready)
			var score sql.NullInt64
			var att sql.NullInt64
			e := a.db.QueryRowContext(r.Context(), `SELECT score,attested_by FROM scores WHERE match_id=$1 AND player_id=$2`, mv.ID, id).Scan(&score, &att)
			if e == nil && score.Valid {
				x.Submitted = true
				x.Score = int(score.Int64)
				x.Attested = att.Valid && att.Int64 > 0
			}
			players = append(players, x)
			if x.Announcement {
				mv.AnnouncementCount++
			}
			if x.Ready {
				mv.ReadyCount++
			}
			if id == pid {
				mv.Announced = x.Announcement
				mv.Ready = x.Ready
				if x.Submitted {
					mv.ScoreSubmitted = true
					mv.MyScore = x.Score
					mv.MyScoreAttested = x.Attested
				}
			}
		}
		for i := range players {
			if players[i].ID == a1 || players[i].ID == b1 {
				players[i].Team = 1
			} else {
				players[i].Team = 2
			}
		}
		for i := range players {
			for _, candidate := range players {
				if candidate.Team != players[i].Team {
					players[i].Attesters = append(players[i].Attesters, candidate)
				}
			}
		}
		mv.Players = players
		mv.GameNo = p.GamesPlayed
		if mv.GameNo < 1 {
			mv.GameNo = 1
		}
		for _, x := range players {
			if x.ID == pid {
				continue
			}
			if (pid == a1 || pid == b1) == (x.ID == a1 || x.ID == b1) {
				if (pid == a1 && x.ID == b1) || (pid == b1 && x.ID == a1) || (pid == a2 && x.ID == b2) || (pid == b2 && x.ID == a2) {
					mv.Partner = x
				}
			} else {
				mv.Opponents = append(mv.Opponents, x)
			}
		}
		if mv.Status == "review" {
			mv.ReviewMessage = "The four scores must form two matching pairs, and the recorded teams must have different totals. The admin must review or correct the scores."
		}
		data["Match"] = &mv
	} else {
		data["NoMatch"] = true
		if p.GamesPlayed >= t.GamesTotal {
			data["PlayerDone"] = true
			data["SitoutMessage"] = "You have completed all of your scheduled games. Please do not play any additional games."
		} else if p.Table == 0 {
			var rank int
			_ = a.db.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM players WHERE tournament_id=$1 AND current_table=0 AND current_letter=$2 AND (sitout_order<$3 OR (sitout_order=$3 AND id<$4))`, t.ID, p.Letter, nullableInt(p.GamesPlayed), p.ID).Scan(&rank)
			data["SitoutMessage"] = fmt.Sprintf("You are assigned to Table 0 and are sitting out. Your letter is %s. The admin will notify you when you rotate into Table %d.", p.Letter, func() int {
				var n int
				_ = a.db.QueryRowContext(r.Context(), `SELECT COUNT(*)/4 FROM players WHERE tournament_id=$1`, t.ID).Scan(&n)
				if p.Letter == "A" {
					return 1
				}
				return n
			}())
		} else {
			data["SitoutMessage"] = fmt.Sprintf("Your next assignment is Table %d%s. You are waiting for that table's previous game to finish and for all four players to be assigned.", p.Table, p.Letter)
		}
	}
	if data["WaitForAnnouncement"] == true {
		data["Flash"] = "Please wait until the admin announces the initiation of the next round."
		data["FlashType"] = "warning"
	}
	a.render(w, "play.html", data)
}
func nullableInt(i int) int { return i }

func (a *App) currentTournament(ctx context.Context) (*Tournament, error) {
	var t Tournament
	e := a.db.QueryRowContext(ctx, `SELECT id,name,games_total,status,(SELECT COUNT(*) FROM players WHERE tournament_id=t.id) FROM tournaments t WHERE id=1`).Scan(&t.ID, &t.Name, &t.GamesTotal, &t.Status, &t.PlayerCount)
	if e != nil {
		return nil, e
	}
	return &t, nil
}
func (a *App) players(ctx context.Context, tid int) ([]Player, error) {
	rows, e := a.db.QueryContext(ctx, `SELECT id,name,COALESCE(current_table,0),COALESCE(current_letter,''),games_played,COALESCE(sitout_order,0) FROM players WHERE tournament_id=$1 ORDER BY id`, tid)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	var out []Player
	for rows.Next() {
		var p Player
		if e = rows.Scan(&p.ID, &p.Name, &p.Table, &p.Letter, &p.GamesPlayed, &p.SitoutOrder); e != nil {
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
	t, e := template.New("layout").Funcs(template.FuncMap{"le": func(a, b int) bool { return a <= b }, "mul": func(a, b int) int { return a * b }}).Parse(string(layout) + "\n" + string(page))
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
		return fmt.Sprintf("%03d", time.Now().UnixNano()%1000)
	}
	v := uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
	return fmt.Sprintf("%03d", v%1000)
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
