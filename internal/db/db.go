// Package db handles all Postgres interaction for the MSO scraper.
// We use pgx directly (no ORM) — straightforward for this kind of pipeline.
package db

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/BigRedS/mso-planner/internal/models"
)

// Pool wraps a pgxpool connection pool with our application methods.
type Pool struct {
	pool *pgxpool.Pool
}

// Connect opens a connection pool to Postgres.
// dsn example: "postgres://mso:password@localhost:5432/mso?sslmode=disable"
func Connect(ctx context.Context, dsn string) (*Pool, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("opening pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		return nil, fmt.Errorf("pinging database: %w", err)
	}
	return &Pool{pool: pool}, nil
}

func (p *Pool) Close() {
	p.pool.Close()
}

// Migrate creates the schema if it doesn't exist.
// We keep migrations inline here for simplicity; for a real project
// you'd use golang-migrate or similar.
func (p *Pool) Migrate(ctx context.Context) error {
	_, err := p.pool.Exec(ctx, schema)
	return err
}

const schema = `
CREATE TABLE IF NOT EXISTS service_areas (
    id              BIGSERIAL PRIMARY KEY,
    slug            TEXT        NOT NULL UNIQUE,
    name            TEXT        NOT NULL,
    mso_url         TEXT        NOT NULL,      -- attribution: always link back here
    road            TEXT        NOT NULL,
    direction       TEXT        NOT NULL DEFAULT 'both',
    location_raw    TEXT,                      -- original MSO location string
    postcode        TEXT,
    address         TEXT,
    latitude        DOUBLE PRECISION,
    longitude       DOUBLE PRECISION,
    operator        TEXT,
    mso_rating      REAL        DEFAULT 0,
    last_scraped_at TIMESTAMPTZ
);

-- Facilities are in a separate table for clean querying.
-- One row per service_area (1:1 relationship but kept separate for clarity).
CREATE TABLE IF NOT EXISTS facilities (
    service_area_id BIGINT PRIMARY KEY REFERENCES service_areas(id) ON DELETE CASCADE,

    -- Stored as text arrays for easy querying: WHERE 'Costa Coffee' = ANY(catering_brands)
    catering_brands     TEXT[]  DEFAULT '{}',
    fuel_brands         TEXT[]  DEFAULT '{}',
    forecourt_shops     TEXT[]  DEFAULT '{}',
    hotels              TEXT[]  DEFAULT '{}',

    has_showers         BOOLEAN DEFAULT FALSE,
    has_atm             BOOLEAN DEFAULT FALSE,
    has_play_area       BOOLEAN DEFAULT FALSE,
    has_lpg             BOOLEAN DEFAULT FALSE,
    hgv_access          BOOLEAN DEFAULT FALSE,
    ev_charging         BOOLEAN DEFAULT FALSE,
    ev_charger_info     TEXT,
    outdoor_space       BOOLEAN DEFAULT FALSE,
    outdoor_space_info  TEXT,
    dog_friendly        BOOLEAN DEFAULT FALSE,
    parking_info        TEXT
);

-- Road memberships: a service at a junction belongs to multiple roads.
-- The scraper primary-keys on the first road seen; this table captures all.
CREATE TABLE IF NOT EXISTS service_roads (
    service_area_id BIGINT  REFERENCES service_areas(id) ON DELETE CASCADE,
    road            TEXT    NOT NULL,
    PRIMARY KEY (service_area_id, road)
);

-- Sources table: ready for future enrichment (TripAdvisor, Google etc.)
CREATE TABLE IF NOT EXISTS service_sources (
    id              BIGSERIAL PRIMARY KEY,
    service_area_id BIGINT      REFERENCES service_areas(id) ON DELETE CASCADE,
    source          TEXT        NOT NULL,  -- 'mso', 'tripadvisor', 'google'
    external_id     TEXT,
    external_url    TEXT,
    rating          REAL,
    review_count    INTEGER,
    fetched_at      TIMESTAMPTZ
);

-- Geocoding results, so we hit Nominatim at most once per distinct query.
-- query is normalised (lower-cased, whitespace collapsed) by the geo package.
CREATE TABLE IF NOT EXISTS geocode_cache (
    query       TEXT             PRIMARY KEY,
    latitude    DOUBLE PRECISION NOT NULL,
    longitude   DOUBLE PRECISION NOT NULL,
    fetched_at  TIMESTAMPTZ      NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_service_areas_road     ON service_areas(road);
CREATE INDEX IF NOT EXISTS idx_service_areas_operator ON service_areas(operator);
CREATE INDEX IF NOT EXISTS idx_service_areas_rating   ON service_areas(mso_rating);

ALTER TABLE service_areas ADD COLUMN IF NOT EXISTS latitude DOUBLE PRECISION;
ALTER TABLE service_areas ADD COLUMN IF NOT EXISTS longitude DOUBLE PRECISION;
CREATE INDEX IF NOT EXISTS idx_service_areas_latlon   ON service_areas(latitude, longitude);
CREATE INDEX IF NOT EXISTS idx_service_roads_road     ON service_roads(road);
`

// UpsertServiceArea inserts or updates a service area and its facilities.
// We upsert on slug so re-running the scraper updates existing records cleanly.
func (p *Pool) UpsertServiceArea(ctx context.Context, sa models.ServiceArea) error {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	// Upsert core service_areas row
	var id int64
	err = tx.QueryRow(ctx, `
		INSERT INTO service_areas
			(slug, name, mso_url, road, direction, location_raw, postcode, address, latitude, longitude, operator, mso_rating, last_scraped_at)
		VALUES
			($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, NOW())
		ON CONFLICT (slug) DO UPDATE SET
			name            = EXCLUDED.name,
			mso_url         = EXCLUDED.mso_url,
			road            = EXCLUDED.road,
			direction       = EXCLUDED.direction,
			location_raw    = EXCLUDED.location_raw,
			postcode        = EXCLUDED.postcode,
			address         = EXCLUDED.address,
			latitude        = EXCLUDED.latitude,
			longitude       = EXCLUDED.longitude,
			operator        = EXCLUDED.operator,
			mso_rating      = EXCLUDED.mso_rating,
			last_scraped_at = NOW()
		RETURNING id
	`,
		sa.Slug, sa.Name, sa.MSOURL, sa.Road, sa.Direction,
		sa.Location, sa.Postcode, sa.Address, sa.Latitude, sa.Longitude, sa.Operator, sa.MSOrating,
	).Scan(&id)
	if err != nil {
		return fmt.Errorf("upserting service area %s: %w", sa.Slug, err)
	}

	// Upsert facilities
	f := sa.Facilities
	_, err = tx.Exec(ctx, `
		INSERT INTO facilities
			(service_area_id, catering_brands, fuel_brands, forecourt_shops, hotels,
			 has_showers, has_atm, has_play_area, has_lpg, hgv_access,
			 ev_charging, ev_charger_info, outdoor_space, outdoor_space_info, dog_friendly)
		VALUES
			($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)
		ON CONFLICT (service_area_id) DO UPDATE SET
			catering_brands    = EXCLUDED.catering_brands,
			fuel_brands        = EXCLUDED.fuel_brands,
			forecourt_shops    = EXCLUDED.forecourt_shops,
			hotels             = EXCLUDED.hotels,
			has_showers        = EXCLUDED.has_showers,
			has_atm            = EXCLUDED.has_atm,
			has_play_area      = EXCLUDED.has_play_area,
			has_lpg            = EXCLUDED.has_lpg,
			hgv_access         = EXCLUDED.hgv_access,
			ev_charging        = EXCLUDED.ev_charging,
			ev_charger_info    = EXCLUDED.ev_charger_info,
			outdoor_space      = EXCLUDED.outdoor_space,
			outdoor_space_info = EXCLUDED.outdoor_space_info,
			dog_friendly       = EXCLUDED.dog_friendly
	`,
		id,
		stringsToArray(f.CateringBrands),
		stringsToArray(f.FuelBrands),
		stringsToArray(f.ForecourtShops),
		stringsToArray(f.Hotels),
		f.HasShowers, f.HasATM, f.HasPlayArea, f.HasLPG, f.HGVAccess,
		f.EVCharging, f.EVChargerInfo,
		f.OutdoorSpace, f.OutdoorSpaceInfo, f.DogFriendly,
	)
	if err != nil {
		return fmt.Errorf("upserting facilities for %s: %w", sa.Slug, err)
	}

	// Record this road in service_roads (ignore conflict — may already exist)
	_, err = tx.Exec(ctx, `
		INSERT INTO service_roads (service_area_id, road)
		VALUES ($1, $2)
		ON CONFLICT DO NOTHING
	`, id, sa.Road)
	if err != nil {
		return fmt.Errorf("inserting service_road for %s: %w", sa.Slug, err)
	}

	return tx.Commit(ctx)
}

func (p *Pool) UpdateServiceCoordinates(ctx context.Context, slug string, lat, lon float64) error {
	_, err := p.pool.Exec(ctx, `
		UPDATE service_areas
		SET latitude = $1, longitude = $2
		WHERE slug = $3
	`, lat, lon, slug)
	return err
}

// GetGeocode returns a cached geocoding result. ok is false on a cache miss.
func (p *Pool) GetGeocode(ctx context.Context, query string) (lat, lon float64, ok bool, err error) {
	err = p.pool.QueryRow(ctx,
		`SELECT latitude, longitude FROM geocode_cache WHERE query = $1`, query,
	).Scan(&lat, &lon)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, 0, false, nil
	}
	if err != nil {
		return 0, 0, false, err
	}
	return lat, lon, true, nil
}

// PutGeocode stores a geocoding result, replacing any existing entry.
func (p *Pool) PutGeocode(ctx context.Context, query string, lat, lon float64) error {
	_, err := p.pool.Exec(ctx, `
		INSERT INTO geocode_cache (query, latitude, longitude, fetched_at)
		VALUES ($1, $2, $3, NOW())
		ON CONFLICT (query) DO UPDATE SET
			latitude   = EXCLUDED.latitude,
			longitude  = EXCLUDED.longitude,
			fetched_at = NOW()
	`, query, lat, lon)
	return err
}

// Stats returns a quick summary of what's in the database — useful after a scrape run.
func (p *Pool) Stats(ctx context.Context) (total int, withFuel int, withEV int, err error) {
	err = p.pool.QueryRow(ctx, `SELECT COUNT(*) FROM service_areas`).Scan(&total)
	if err != nil {
		return
	}
	err = p.pool.QueryRow(ctx, `SELECT COUNT(*) FROM facilities WHERE array_length(fuel_brands, 1) > 0`).Scan(&withFuel)
	if err != nil {
		return
	}
	err = p.pool.QueryRow(ctx, `SELECT COUNT(*) FROM facilities WHERE ev_charging = TRUE`).Scan(&withEV)
	return
}

// stringsToArray converts a Go string slice to a pgx-compatible array string.
// pgx handles []string directly when the column is TEXT[].
func stringsToArray(ss []string) interface{} {
	if len(ss) == 0 {
		return []string{}
	}
	// pgx v5 accepts []string directly for TEXT[] columns
	return ss
}

// QueryByRoad returns all service areas for a given road, ordered north/east first.
// This is a helper for testing/debugging; the main API will have richer queries.
func (p *Pool) QueryByRoad(ctx context.Context, road string) ([]models.ServiceArea, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT sa.slug, sa.name, sa.mso_url, sa.road, sa.direction,
		       sa.location_raw, sa.postcode, sa.operator, sa.mso_rating
		FROM service_areas sa
		WHERE sa.road = $1
		ORDER BY sa.name
	`, strings.ToUpper(road))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []models.ServiceArea
	for rows.Next() {
		var sa models.ServiceArea
		if err := rows.Scan(
			&sa.Slug, &sa.Name, &sa.MSOURL, &sa.Road, &sa.Direction,
			&sa.Location, &sa.Postcode, &sa.Operator, &sa.MSOrating,
		); err != nil {
			return nil, err
		}
		out = append(out, sa)
	}
	return out, rows.Err()
}

type ListServicesOptions struct {
	Road     string
	Roads    []string
	Operator string
	WithFuel bool
	WithEV   bool
	Limit    int
}

func (p *Pool) ListServices(ctx context.Context, opts ListServicesOptions) ([]models.ServiceArea, error) {
	query := `
		SELECT DISTINCT sa.slug, sa.name, sa.mso_url, sa.road, sa.direction,
		       sa.location_raw, sa.postcode, sa.address, COALESCE(sa.latitude, 0), COALESCE(sa.longitude, 0), sa.operator, sa.mso_rating,
		       sa.last_scraped_at::text,
		       f.catering_brands, f.fuel_brands, f.forecourt_shops, f.hotels,
		       f.has_showers, f.has_atm, f.has_play_area, f.has_lpg, f.hgv_access,
		       f.ev_charging, f.ev_charger_info, f.outdoor_space, f.outdoor_space_info,
		       f.dog_friendly
		FROM service_areas sa
		JOIN facilities f ON f.service_area_id = sa.id
		LEFT JOIN service_roads sr ON sr.service_area_id = sa.id
	`
	clauses := []string{"1=1"}
	args := []interface{}{}

	if opts.WithFuel {
		clauses = append(clauses, `array_length(f.fuel_brands, 1) > 0`)
	}
	if opts.WithEV {
		clauses = append(clauses, `f.ev_charging = TRUE`)
	}
	if len(opts.Roads) > 0 {
		args = append(args, opts.Roads)
		clauses = append(clauses, fmt.Sprintf("sr.road = ANY($%d)", len(args)))
	}
	if opts.Road != "" {
		args = append(args, strings.ToUpper(opts.Road))
		clauses = append(clauses, fmt.Sprintf("sr.road = $%d", len(args)))
	}
	if opts.Operator != "" {
		args = append(args, "%"+opts.Operator+"%")
		clauses = append(clauses, fmt.Sprintf("sa.operator ILIKE $%d", len(args)))
	}

	if opts.Limit <= 0 {
		opts.Limit = 200
	} else if opts.Limit > 1000 {
		opts.Limit = 1000
	}
	args = append(args, opts.Limit)
	query += " WHERE " + strings.Join(clauses, " AND ") + fmt.Sprintf(" ORDER BY sa.name LIMIT $%d", len(args))

	rows, err := p.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []models.ServiceArea
	for rows.Next() {
		var sa models.ServiceArea
		if err := rows.Scan(
			&sa.Slug, &sa.Name, &sa.MSOURL, &sa.Road, &sa.Direction,
			&sa.Location, &sa.Postcode, &sa.Address, &sa.Latitude, &sa.Longitude, &sa.Operator, &sa.MSOrating,
			&sa.LastScrapedAt,
			&sa.Facilities.CateringBrands,
			&sa.Facilities.FuelBrands,
			&sa.Facilities.ForecourtShops,
			&sa.Facilities.Hotels,
			&sa.Facilities.HasShowers,
			&sa.Facilities.HasATM,
			&sa.Facilities.HasPlayArea,
			&sa.Facilities.HasLPG,
			&sa.Facilities.HGVAccess,
			&sa.Facilities.EVCharging,
			&sa.Facilities.EVChargerInfo,
			&sa.Facilities.OutdoorSpace,
			&sa.Facilities.OutdoorSpaceInfo,
			&sa.Facilities.DogFriendly,
		); err != nil {
			return nil, err
		}
		out = append(out, sa)
	}
	return out, rows.Err()
}

// Ensure pgx is used (suppress unused import if QueryByRoad isn't called yet)
var _ = pgx.Identifier{}
