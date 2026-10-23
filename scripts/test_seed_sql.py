from pathlib import Path
import json
import os
import re
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch

import candidate_skins_seed
import community_resources_seed
import community_seed
import seed_sql
from seed_sql import literal


class SeedSQLTests(unittest.TestCase):
    def test_empty_and_plain_text(self):
        self.assertEqual(literal(""), "''")
        self.assertEqual(literal("plain"), "'plain'")

    def test_quotes_are_doubled(self):
        self.assertEqual(literal("O'Reilly"), "'O''Reilly'")
        self.assertEqual(literal("''"), "''''''")
        self.assertEqual(literal("a'; SELECT 1; --"), "'a''; SELECT 1; --'")

    def test_unicode_backslashes_and_newlines_are_preserved(self):
        self.assertEqual(literal("水杉🙂"), "'水杉🙂'")
        self.assertEqual(literal("a\\b\n第二行\r\n"), "'a\\b\n第二行\r\n'")

    def test_existing_script_entry_points(self):
        for name, quote in (
            ("candidate_skins_seed", candidate_skins_seed.literal),
            ("community_seed", community_seed.literal),
            ("community_resources_seed", community_resources_seed.quoted),
        ):
            with self.subTest(script=name):
                self.assertEqual(quote("合成 ' 文本"), "'合成 '' 文本'")

    def test_package_imports(self):
        result = subprocess.run(
            [sys.executable, "-c", "from scripts import candidate_skins_seed, community_resources_seed, community_seed"],
            cwd=Path(__file__).resolve().parents[1],
            capture_output=True,
            text=True,
        )
        self.assertEqual(result.returncode, 0, result.stderr)

    def check_do_boundaries(self, sql):
        blocks = [line for line in sql.splitlines() if line.startswith("DO ")]
        self.assertTrue(blocks)
        for block in blocks:
            match = re.fullmatch(r"DO (\$[A-Za-z_0-9]*\$) (.*) \1;", block)
            self.assertIsNotNone(match, block)
            delimiter, body = match.groups()
            self.assertNotIn(delimiter, body, "正文不能提前结束外层美元引用")

    def test_community_skin_dollar_quoting(self):
        for field in ("publisher", "name", "description", "design"):
            with self.subTest(field=field):
                skin = {"id": "synthetic-skin", "name": "合成皮肤", "description": "合成描述", "design": {}}
                catalog = {"publisher": {"id": "synthetic-owner", "display_name": "合成发布者"}, "skins": [skin]}
                value = "合成 $$ $seed$ $seed_1$ O'Reilly"
                if field == "publisher":
                    catalog["publisher"]["display_name"] = value
                else:
                    skin[field] = {"note": value} if field == "design" else value
                self.check_do_boundaries(community_seed.render(catalog))

    def test_community_resource_dollar_quoting(self):
        for field in ("name", "description", "content"):
            with self.subTest(field=field), tempfile.TemporaryDirectory() as directory:
                root = Path(directory)
                (root / "assets").mkdir()
                item = {"slug": "synthetic-reply", "kind": "reply", "name": "合成回复", "description": "合成描述", "content": {}}
                value = "合成 $$ $seed$ $seed_1$ O'Reilly"
                item[field] = {"text": value} if field == "content" else value
                (root / "assets/community-starter-resources.json").write_text(json.dumps([item]), encoding="utf-8")
                with patch.object(community_resources_seed, "root", root):
                    self.check_do_boundaries(community_resources_seed.render())

    def test_do_block_preserves_its_body(self):
        for body in ("BEGIN PERFORM 1; END", "BEGIN PERFORM '$$ $seed_1$ $seed_2$'; END", "BEGIN\nPERFORM '水杉 O''Reilly';\nEND"):
            with self.subTest(body=body):
                block = seed_sql.do_block(body)
                match = re.fullmatch(r"DO (\$[A-Za-z_0-9]*\$) (.*) \1;", block, re.DOTALL)
                self.assertIsNotNone(match)
                self.assertEqual(match[2], body)
                self.assertNotIn(match[1], body)

    def test_do_block_executes_in_postgresql(self):
        database = os.environ.get("MSIME_TEST_DATABASE_URL")
        if not database:
            self.skipTest("需要 MSIME_TEST_DATABASE_URL 和 psql")
        values = ["普通文本", "O'Reilly", "$$", "$seed$ $$ $seed_1$", "$$; SELECT 99; DO $$", "水杉🙂\\路径\n第二行"]
        statements = ["CREATE TEMP TABLE seed_sql_values(id integer, value text);"]
        for index, value in enumerate(values):
            statements.append(seed_sql.do_block(f"BEGIN INSERT INTO seed_sql_values VALUES({index},{literal(value)}); END"))
        statements.append("SELECT json_agg(value ORDER BY id) FROM seed_sql_values;")
        result = subprocess.run(
            ["psql", "-X", "-qAt", "-v", "ON_ERROR_STOP=1", "--dbname", database],
            input="\n".join(statements), capture_output=True, text=True, timeout=30,
        )
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(json.loads(result.stdout), values)


if __name__ == "__main__":
    unittest.main()
