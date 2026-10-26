# Euchre Tournament Scorer

Go web app for individual Euchre scoring. It stores data in Neon PostgreSQL (not Render's ephemeral filesystem), supports one current tournament, and exports CSV before the organizer deletes it.

## Features
- 1–24 games and player roster entered by the organizer
- Admin account setup on first visit; bcrypt password hashes
- Per-player PINs; PINs are shown to the organizer during setup and cleared from the database when the tournament starts
- Scores are 1–32; duplicate player/game submissions are prevented by server checks and a PostgreSQL unique constraint
- Players cannot edit scores after submission; admin can correct submitted scores with an audit record
- Each table progresses independently after the admin announcement and all four players confirm readiness
- Admin-assigned starting table/letter entries, including ordered Table 0 sit-out queues; a two-person queue works identically for A or B
- Opponent PIN attestation for score submissions and partnership score consistency checks
- Players stop participating after completing their configured number of games; the app blocks game-count combinations that cannot mathematically give every player exactly that many games
- Admin scheduling alerts identify idle tables that cannot form two A and two B players; the admin can move a waiting player or change their letter without interrupting any waiting/active game
- Live leaderboard polls every 10 seconds
- QR code for player login
- CSV export, including per-game scores, completed games, total, and average
- New tournament deletes the current tournament, players, scores, sessions, and corrections; the admin account remains
- Responsive/mobile-friendly pages

## Deploy on Render Free + Neon Free
1. Create a PostgreSQL project in Neon and copy its **pooled** connection string. Keep `sslmode=require` in the connection string.
2. Push this project to a GitHub repository.
3. In Render choose **New → Blueprint** and select the repository containing `render.yaml`, or create a Go Web Service manually.
4. Select the **Free** plan. Ensure `DATABASE_URL` is set to the Neon connection string and `SESSION_SECRET` is a long random secret (the Blueprint generates it).
5. Build command: `go build -o app .`; start command: `./app`.
6. Open the deployed URL. First visit asks you to create the admin account.

## Local run
Install Go 1.23 or newer. Set environment variables, then run `go run .`.
PowerShell example:
```powershell
$env:DATABASE_URL="your-neon-connection-string"
$env:SESSION_SECRET="use-a-long-random-secret-here"
go run .
```
Open `http://localhost:10000`.

## Tournament workflow
1. Admin creates a tournament, chooses 1–24 games, and enters one player per line.
2. The admin dashboard shows each player's PIN. Give each PIN to its player privately.
3. Give each player their assigned starting table and A/B letter. Players log in and enter that assignment, or the admin may enter/correct assignments on the dashboard. Validate that each active table has two A players and two B players, and that Table 0 has the right number and letter split. The app warns when player count × games per player is not divisible by four and blocks starting until the game count is adjusted; letter changes cannot fix that global arithmetic constraint.
4. Start the tournament. The roster and assignments are then locked and setup PINs are removed from the database.
5. Players use the QR code or `/play`, select their name, and enter their 3-digit PIN.
6. Players acknowledge the admin announcement and individually confirm readiness. A game begins only after all four assigned players confirm. Each player submits their score (1–32), and a player on the opposing team attests it with their own PIN.
7. If an idle table cannot form a game, review the admin scheduling alert and use Admin Position Override to move a waiting player or change their A/B letter. The app will rebuild eligible lineups, while any game already underway remains unchanged.
8. Export CSV before choosing **Delete Current Tournament & Start Fresh**.

## Free-tier caveats
Render Free web services sleep after 15 minutes without inbound traffic and can take about a minute to wake. Open player/leaderboard pages poll while being used, but a quiet break can still cause a cold start. Render Free has an ephemeral filesystem, so this project deliberately stores all persistent data in Neon. Neon and Render can change their free-tier limits; check their current plan terms before relying on the service for an event. Keep CSV exports as your permanent records.
