BEGIN;

CREATE TABLE content_filter_transactions (
    singleton BOOLEAN PRIMARY KEY DEFAULT true CHECK (singleton),
    operation_id UUID NOT NULL UNIQUE,
    source_network TEXT NOT NULL,
    snapshot JSONB NOT NULL CHECK (jsonb_typeof(snapshot) = 'object'),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE content_filter_canary_reservations (
    source_network CIDR PRIMARY KEY,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (family(source_network) = 4),
    CHECK (masklen(source_network) = 24),
    CHECK (source_network <<= '10.100.0.0/16'::cidr)
);

COMMIT;
