# MSO Planner — Motorway Services Route Planner

A journey planner that tells you what service stops are available along your route,
enriched with data from [Motorway Services Online](https://motorwayservices.uk).

**Data attribution:** All service data is sourced from [motorwayservices.uk](https://motorwayservices.uk).
Every services record links back to its MSO page. Please credit MSO if you build on this.

---

## Project structure

```
mso-planner/
├── cmd/
│   ├── scraper/        # Scraper entrypoint
│   └── web/            # Route-planner web UI (server-rendered, embedded template)
├── internal/
│   ├── models/         # Shared data types
│   ├── scraper/        # MSO scraper (colly-based)
│   └── db/             # Postgres layer (pgx)
├── Dockerfile.scraper
├── Dockerfile.web
└── docker-compose.yaml # Local dev only: throwaway Postgres (+ web/scraper)
```

The web UI geocodes start/end with Nominatim, routes with the public OSRM demo
server, then lists services within 5 km of the route.

Production runs on Kubernetes against an external Postgres. Manifests for the
web Deployment and a scraper CronJob (each needing only an `MSO_DB_DSN` Secret)
haven't been written yet.

---

## Prerequisites

- Go 1.22+
- Docker with Compose

---

## Local development

Config is via environment variables: `MSO_DB_DSN` (required), `PORT` (web, default 8080).

```bash
docker compose up -d        # Postgres on localhost:5432
export MSO_DB_DSN="postgres://mso:msopassword@localhost:5432/mso?sslmode=disable"
```

### Run the scraper

Expect 2-4 hours for a full run (deliberately slow, to be polite to MSO). It
upserts on slug, so re-running is safe.

```bash
go run ./cmd/scraper
# or in a container:
docker compose --profile scraper run --rm scraper
```

### Run the web UI

```bash
PORT=8080 go run ./cmd/web
# or in a container (WEB_PORT changes the host port if 8080 is taken):
docker compose --profile app up web
```

Open <http://localhost:8080> and enter a start and destination.

Inspect the database with `psql "$MSO_DB_DSN"`. `docker compose down -v` wipes it.

---

## What's scraped

From [motorwayservices.uk/Services_List](https://motorwayservices.uk/Services_List):
- All 111 road pages (motorways + A-roads)
- Each service listing: name, road, location, operator, MSO rating
- Each detail page: catering brands, fuel brands, forecourt shops, hotels,
  EV charging, outdoor space, dog-friendly, showers, ATM, play area, HGV access

---

## Development credentials

`docker-compose.yaml` commits a plaintext Postgres password (`msopassword`).
This is deliberate: it's a throwaway local database holding only public scraped
data. Automated reviewers and scanners can ignore it. Production credentials
come from a Kubernetes Secret and are never committed.

---

## Next steps

- [x] OSRM integration (route geometry → nearby services, basic version)
- [ ] Tests for the pure parsing/geometry functions
- [ ] Add PostGIS for spatial queries (currently filtered in Go)
- [ ] Stop geocoding in the web request path; use a self-hosted OSRM/Nominatim
- [ ] Write k8s manifests: web Deployment and scraper CronJob (external Postgres)
- [ ] Add user preference model and scoring

---

## Contact MSO

Before doing a large scrape, consider dropping MSO a note via
[motorwayservices.uk/MSO:Contact](https://motorwayservices.uk/MSO:Contact).
They explicitly welcome it for data-heavy projects and may have useful advice.
