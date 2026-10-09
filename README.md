# Euchre Tournament Scorer

Go web app for individual Euchre scoring. It stores data in Neon PostgreSQL (not Render's ephemeral filesystem), supports one current tournament, and exports CSV before the organizer deletes it.

## Features
- 1–24 games and player roster entered by the organizer
- Admin account setup on first visit; bcrypt password hashes
- Per-player PINs; PINs are shown to the organizer during setup and cleared from the database when the tournament starts
- Scores are 1–40; duplicate player/game submissions are prevented by server checks and a PostgreSQL unique constraint
- Players cannot edit scores after submission; admin can correct submitted scores with an audit record
- Game N is server-locked until every player has submitted Game N-1
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
3. Start the tournament. The roster is then locked and setup PINs are removed from the database.
4. Players use the QR code or `/play`, select their name, and enter their PIN.
5. Each player submits one score for the currently unlocked game. The next game cannot be submitted until all players have submitted the current one.
6. Export CSV before choosing **Delete Current Tournament & Start Fresh**.

## Free-tier caveats
Render Free web services sleep after 15 minutes without inbound traffic and can take about a minute to wake. Open player/leaderboard pages poll while being used, but a quiet break can still cause a cold start. Render Free has an ephemeral filesystem, so this project deliberately stores all persistent data in Neon. Neon and Render can change their free-tier limits; check their current plan terms before relying on the service for an event. Keep CSV exports as your permanent records.
