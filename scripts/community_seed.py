#!/usr/bin/env python3
"""Render the reviewed starter catalog as idempotent SQL; never opens a DB connection."""
import json
from pathlib import Path

if __package__:
    from .seed_sql import do_block, literal
else:
    from seed_sql import do_block, literal


def render(catalog):
    owner = literal(catalog["publisher"]["id"])
    name = literal(catalog["publisher"]["display_name"])
    statements = ["BEGIN;", "SET LOCAL ROLE msime_backend;", "SET LOCAL lock_timeout = '5s';", "SET LOCAL statement_timeout = '30s';",
        f"SELECT pg_advisory_xact_lock(hashtext({owner}));",
        f"INSERT INTO auth_users(id,display_name) VALUES({owner},{name}) ON CONFLICT(id) DO NOTHING;",
        do_block(f"BEGIN IF NOT EXISTS (SELECT 1 FROM auth_users WHERE id={owner} AND display_name={name}) "
        f"OR EXISTS (SELECT 1 FROM auth_identities WHERE user_id={owner}) "
        f"OR EXISTS (SELECT 1 FROM auth_sessions WHERE user_id={owner}) "
        "THEN RAISE EXCEPTION 'Starter publisher conflicts with an existing account'; END IF; END")]
    for skin in catalog["skins"]:
        sid, title, desc = (literal(skin[k]) for k in ("id", "name", "description"))
        design = literal(json.dumps(skin["design"], ensure_ascii=False, separators=(",", ":"))) + "::jsonb"
        statements += [
            f"INSERT INTO community_skins(id,owner_id,name,description,design) VALUES({sid},{owner},{title},{desc},{design}) ON CONFLICT(id) DO NOTHING;",
            do_block(f"BEGIN IF NOT EXISTS (SELECT 1 FROM community_skins WHERE id={sid} AND owner_id={owner} AND name={title} AND description={desc} AND design={design}) "
            "THEN RAISE EXCEPTION 'Starter design conflicts; publish a new version instead of overwriting'; END IF; END")
        ]
    statements += [f"SELECT count(*) AS starter_skins FROM community_skins WHERE owner_id={owner};", "COMMIT;"]
    return "\n".join(statements)


if __name__ == "__main__":
    print(render(json.loads((Path(__file__).resolve().parents[1] / "assets/community-starter-skins.json").read_text())))
