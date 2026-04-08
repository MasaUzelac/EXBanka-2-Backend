-- =============================================================================
-- Migration: 000030_add_fund_tables (UP)
-- Adds investment fund and fund-position tables.
-- =============================================================================

CREATE TABLE core_banking.investment_funds (
    id          BIGINT         GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    name        VARCHAR(255)   NOT NULL,
    description TEXT           NOT NULL DEFAULT '',
    manager_id  BIGINT         NOT NULL,  -- supervisor user_id (cross-service ref)
    created_at  TIMESTAMPTZ    NOT NULL DEFAULT NOW()
);

CREATE TABLE core_banking.fund_positions (
    id           BIGINT         GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    fund_id      BIGINT         NOT NULL REFERENCES core_banking.investment_funds(id),
    user_id      BIGINT         NOT NULL,
    account_id   BIGINT         NOT NULL REFERENCES core_banking.racun(id),
    invested_rsd NUMERIC(18, 4) NOT NULL DEFAULT 0,
    created_at   TIMESTAMPTZ    NOT NULL DEFAULT NOW(),

    CONSTRAINT uq_fund_position UNIQUE (fund_id, user_id)
);

CREATE INDEX idx_fund_positions_user ON core_banking.fund_positions (user_id);
CREATE INDEX idx_fund_positions_fund ON core_banking.fund_positions (fund_id);
CREATE INDEX idx_investment_funds_manager ON core_banking.investment_funds (manager_id);
