-- =============================================================================
-- Migration: 000029_add_portfolio_tax_tables (UP)
-- Adds tables required for portfolio tax tracking and OTC public shares.
-- =============================================================================

-- monthly capital-gains tax debt per user / account
CREATE TABLE core_banking.tax_records (
    id          BIGINT         GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    user_id     BIGINT         NOT NULL,
    account_id  BIGINT         NOT NULL REFERENCES core_banking.racun(id),
    year        INTEGER        NOT NULL,
    month       INTEGER        NOT NULL,  -- 1-12
    amount_rsd  NUMERIC(18, 4) NOT NULL DEFAULT 0,
    paid        BOOLEAN        NOT NULL DEFAULT FALSE,
    paid_at     TIMESTAMPTZ,
    created_at  TIMESTAMPTZ    NOT NULL DEFAULT NOW(),

    CONSTRAINT chk_tax_month CHECK (month BETWEEN 1 AND 12),
    CONSTRAINT uq_tax_user_account_period UNIQUE (user_id, account_id, year, month)
);

CREATE INDEX idx_tax_records_user_id  ON core_banking.tax_records (user_id);
CREATE INDEX idx_tax_records_paid     ON core_banking.tax_records (paid) WHERE paid = FALSE;

-- shares a user has made publicly visible for OTC trading
CREATE TABLE core_banking.public_shares (
    id          BIGINT         GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    listing_id  BIGINT         NOT NULL REFERENCES core_banking.listing(id),
    user_id     BIGINT         NOT NULL,
    quantity    INTEGER        NOT NULL,
    created_at  TIMESTAMPTZ    NOT NULL DEFAULT NOW(),

    CONSTRAINT chk_public_shares_qty CHECK (quantity > 0)
);

CREATE INDEX idx_public_shares_user    ON core_banking.public_shares (user_id);
CREATE INDEX idx_public_shares_listing ON core_banking.public_shares (listing_id);
