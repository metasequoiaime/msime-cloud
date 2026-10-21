from pathlib import Path
import subprocess
import sys
import unittest

import candidate_skins_seed
import community_resources_seed
import community_seed
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


if __name__ == "__main__":
    unittest.main()
