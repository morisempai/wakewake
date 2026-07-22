-- 0001_product.sql — the catalog's product table.
--
-- Catalog is read-mostly: it owns product identity and display pricing, and nothing else. It does
-- NOT own availability (that is the availability service's exclusion constraint) and its prices are
-- indicative — Payment computes the authoritative charge. Nothing here writes; rows arrive by seed
-- or by an out-of-band admin path that is out of scope for this slice.

CREATE TYPE product_type AS ENUM ('boat', 'yacht', 'wakesurf_session');

CREATE TABLE product (
  id               uuid PRIMARY KEY,

  -- The bookable resource this product maps to. Availability enforces its no-overlap invariant
  -- against this id, never against the product id (see contracts/openapi/catalog.yaml). Catalog
  -- only records the handle; it does not reserve against it.
  resource_id      uuid NOT NULL,

  type             product_type NOT NULL,
  name             text NOT NULL,
  description      text,

  -- Maximum number of people. The `min_capacity` filter selects rows whose capacity is at least
  -- the requested party size, so this is the seating ceiling, not a fixed party size.
  capacity         integer NOT NULL,

  -- Free-text location slug, e.g. "lake-geneva". The contract filters on an exact match.
  location         text NOT NULL,

  -- Indicative price in the currency's minor unit (e.g. cents). Integer to avoid float rounding,
  -- and display-only: Payment computes the authoritative amount. bigint so a high-value yacht in a
  -- minor unit cannot overflow a 32-bit column.
  base_price_minor bigint NOT NULL,

  -- ISO 4217, uppercase three-letter code. The contract's Product schema pins the ^[A-Z]{3}$ shape.
  currency         text NOT NULL,

  -- Optional media handles. Stored as an array rather than a child table because they are read as a
  -- whole with the product, never queried across products, and never mutated field by field here.
  media_urls       text[] NOT NULL DEFAULT '{}',

  created_at       timestamptz NOT NULL DEFAULT now(),
  updated_at       timestamptz NOT NULL DEFAULT now(),

  -- The contract's field constraints, enforced at the source of truth rather than trusted to the
  -- (currently non-existent) write path. A row that violates the OpenAPI schema must never exist,
  -- because every read serves it back verbatim and a bad row is an un-serveable response.
  CONSTRAINT product_name_len         CHECK (char_length(name) BETWEEN 1 AND 200),
  CONSTRAINT product_description_len   CHECK (description IS NULL OR char_length(description) <= 5000),
  CONSTRAINT product_capacity_min      CHECK (capacity >= 1),
  CONSTRAINT product_location_len      CHECK (char_length(location) BETWEEN 1 AND 100),
  CONSTRAINT product_base_price_min    CHECK (base_price_minor >= 0),
  CONSTRAINT product_currency_iso4217  CHECK (currency ~ '^[A-Z]{3}$')
);

-- Newest-first keyset pagination scans this index in reverse. The (created_at, id) pair is the sort
-- key AND the cursor: id (a UUIDv7, itself time-ordered) breaks ties when two products share a
-- created_at, so the ordering is total and a cursor names exactly one boundary row. An OFFSET-based
-- page would shift every time a row is inserted ahead of it; a keyset page anchored to this pair
-- does not (AC3).
CREATE INDEX product_created_at_id_idx ON product (created_at DESC, id DESC);

-- The type filter is low-cardinality but common; a plain btree keeps `WHERE type = $1` off a
-- sequential scan once the catalog grows. location is filtered by exact match for the same reason.
CREATE INDEX product_type_idx     ON product (type);
CREATE INDEX product_location_idx ON product (location);
