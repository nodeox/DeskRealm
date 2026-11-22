-- +goose Up
CREATE TABLE address_books (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id uuid NOT NULL REFERENCES organizations(id),
    owner_id uuid NOT NULL,
    name text NOT NULL CHECK (char_length(name) BETWEEN 1 AND 80),
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (organization_id,id),
    UNIQUE (organization_id,owner_id,name),
    FOREIGN KEY (organization_id,owner_id) REFERENCES users(organization_id,id)
);

CREATE TABLE address_book_entries (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id uuid NOT NULL,
    book_id uuid NOT NULL,
    device_id uuid NOT NULL,
    alias text NOT NULL CHECK (char_length(alias) <= 80),
    tags text[] NOT NULL CHECK (cardinality(tags) <= 12),
    note text NOT NULL CHECK (char_length(note) <= 500),
    version bigint NOT NULL DEFAULT 1 CHECK (version >= 1),
    UNIQUE (book_id,device_id),
    FOREIGN KEY (organization_id,book_id) REFERENCES address_books(organization_id,id),
    FOREIGN KEY (organization_id,device_id) REFERENCES devices(organization_id,id)
);
CREATE INDEX address_book_entries_book_cursor ON address_book_entries(book_id,id);

CREATE TABLE address_book_grants (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id uuid NOT NULL,
    book_id uuid NOT NULL,
    user_id uuid,
    group_id uuid,
    permission smallint NOT NULL CHECK (permission BETWEEN 1 AND 3),
    CHECK ((user_id IS NULL) <> (group_id IS NULL)),
    UNIQUE NULLS NOT DISTINCT (book_id,user_id,group_id),
    FOREIGN KEY (organization_id,book_id) REFERENCES address_books(organization_id,id),
    FOREIGN KEY (organization_id,user_id) REFERENCES users(organization_id,id),
    FOREIGN KEY (organization_id,group_id) REFERENCES user_groups(organization_id,id)
);

-- +goose Down
DROP TABLE address_book_grants,address_book_entries,address_books;
