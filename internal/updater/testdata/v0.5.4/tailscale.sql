-- Source: v0.5.4 internal/tailscale/schema.sql (schema 37).
CREATE TABLE tailscale_settings (
 id INTEGER PRIMARY KEY CHECK(id=1), revision INTEGER NOT NULL,
 tailnet TEXT NOT NULL, token_ciphertext TEXT NOT NULL
);
INSERT INTO tailscale_settings VALUES(1,1,'-','');
