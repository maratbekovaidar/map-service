-- Trips: one car driving to one destination, watched by one viewer.
CREATE TABLE trips (
    id                uuid PRIMARY KEY DEFAULT uuidv7(),
    status            text NOT NULL DEFAULT 'active'
                      CHECK (status IN ('active', 'arrived', 'finished', 'cancelled', 'expired')),
    profile           text NOT NULL,
    origin_lon        double precision NOT NULL,
    origin_lat        double precision NOT NULL,
    dest_lon          double precision NOT NULL,
    dest_lat          double precision NOT NULL,
    -- SHA-256 of the capability tokens; the tokens themselves are never stored.
    driver_token_hash bytea NOT NULL UNIQUE,
    watch_token_hash  bytea NOT NULL UNIQUE,
    route_version     integer NOT NULL DEFAULT 1,
    -- tracker.State: what the tracker remembers between batches of positions.
    tracker_state     jsonb NOT NULL DEFAULT '{}',
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now(),
    -- Server time of the last batch of positions: abandoned trips expire.
    last_seen_at      timestamptz NOT NULL DEFAULT now(),
    ended_at          timestamptz,
    CHECK ((status = 'active') = (ended_at IS NULL))
);
CREATE INDEX trips_active_last_seen ON trips (last_seen_at) WHERE status = 'active';
CREATE INDEX trips_ended_at ON trips (ended_at) WHERE ended_at IS NOT NULL;

-- Every route a trip has had; version 1 is the initial one.
CREATE TABLE trip_routes (
    trip_id       uuid NOT NULL REFERENCES trips (id) ON DELETE CASCADE,
    version       integer NOT NULL CHECK (version > 0),
    reason        text NOT NULL CHECK (reason IN ('initial', 'off_route', 'wrong_way')),
    from_lon      double precision NOT NULL,
    from_lat      double precision NOT NULL,
    heading       double precision,   -- direction of travel it was built for; NULL if unknown
    distance_m    double precision NOT NULL,
    duration_s    double precision NOT NULL,
    geometry      text NOT NULL,      -- encoded polyline, precision 6
    seg_durations double precision[], -- seconds per geometry segment (OSRM annotations)
    data_version  text NOT NULL,      -- map data version it was built on
    created_at    timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (trip_id, version)
);

-- Every position the driver sent, with the tracker's verdict.
CREATE TABLE trip_positions (
    trip_id       uuid NOT NULL REFERENCES trips (id) ON DELETE CASCADE,
    seq           bigint NOT NULL,
    device_time   timestamptz NOT NULL,
    received_at   timestamptz NOT NULL,
    lon           double precision NOT NULL,
    lat           double precision NOT NULL,
    accuracy_m    real,             -- NULL if not reported
    speed_mps     real,
    heading       real,
    route_version integer NOT NULL, -- the route it was judged against
    reject        text,             -- why the tracker ignored it; NULL if accepted
    class         text,             -- on | near | off | wrong_way | start
    along_m       double precision,
    offset_m      double precision,
    PRIMARY KEY (trip_id, seq)
);

-- What watchers receive over SSE, in order; id is the SSE event id. Events
-- of a trip are only written while holding its row lock, so per trip ids are
-- committed in increasing order and "id > Last-Event-ID" misses nothing.
CREATE TABLE trip_events (
    id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    trip_id    uuid NOT NULL REFERENCES trips (id) ON DELETE CASCADE,
    type       text NOT NULL,
    data       jsonb NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX trip_events_trip_id ON trip_events (trip_id, id);
