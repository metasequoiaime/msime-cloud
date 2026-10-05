import contextlib
import io
import json
import os
import subprocess
import tempfile
import unittest
from pathlib import Path

import candidate_skins_seed as seed

FIXTURE = Path(__file__).resolve().parents[1] / "internal/skins/testdata/client_dialect.json"
MANIFEST = "schema_version = 1\nid = '{id}'\nname = 'Harbor'\nversion = '1.0.0'\nbase = '{base}'\n[supports]\nlayouts = ['horizontal']\nthemes = ['dark']\n[candidate_window]\nmin_width_dip = 0\n[candidate_window.background]\nimage = 'assets/bg.png'\n{extra}"


def build_case(fixture, case):
    """Mirror clientFixture.build in internal/skins/client_test.go."""
    sid, manifest, files = "sample", case.get("manifest", ""), []
    if case["template"] != "raw":
        template = fixture["templates"][case["template"]]
        sid, manifest, files = template["id"], template["manifest"], template["files"]
    sid = case.get("id") or sid
    manifest = manifest.replace("{id}", sid)
    for old, new in case.get("replace", []):
        assert old in manifest, (case["name"], old)
        manifest = manifest.replace(old, new, 1)
    manifest = case.get("prepend", "") + manifest + case.get("append", "")
    if case.get("pad_to"):
        manifest += "#" + "x" * (case["pad_to"] - len(manifest.encode()) - 1)
    if case.get("files") is not None:
        files = case["files"]
    return sid, manifest.encode(), {name: 1 for name in files}


class ClientDialectTests(unittest.TestCase):
    def test_shared_fixture_matches_the_go_validator(self):
        fixture = json.loads(FIXTURE.read_text())
        self.assertGreater(len(fixture["cases"]), 150)
        for case in fixture["cases"]:
            with self.subTest(case["name"]):
                sid, manifest, files = build_case(fixture, case)
                try:
                    table = seed.validate(sid, manifest, files)
                    reason = None
                except seed.Invalid as error:
                    reason = str(error)
                self.assertEqual(reason, case["reason"])
                if reason is None and case.get("base"):
                    self.assertEqual(table["base"], case["base"])

    def test_resource_limits(self):
        manifest = b"schema_version = 1\nid = 'sample'\nname = 'S'\nversion = '1'\nbase = 'ink'\n[supports]\nlayouts = ['vertical']\nthemes = ['light']\n[candidate_window]\n"
        seed.validate("sample", manifest, {f"a{i}.css": 1 for i in range(511)})
        for files, reason in [({f"a{i}.css": 1 for i in range(512)}, "too many resources"), ({f"{c}.png": 4 << 20 for c in "abcd"}, "package exceeds 16 MiB"), ({"a.png": (4 << 20) + 1}, "invalid resource: a.png"), ({"a.txt": 1}, "invalid resource: a.txt")]:
            with self.assertRaisesRegex(seed.Invalid, reason):
                seed.validate("sample", manifest, files)
        with self.assertRaisesRegex(seed.Invalid, "not UTF-8"):
            seed.validate("sample", b"\xff", {})
        with self.assertRaisesRegex(seed.Invalid, "invalid TOML"):
            seed.validate("sample", manifest + b"n = 9223372036854775808\n", {})


class SeedTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.root = Path(self.tmp.name)

    def tearDown(self):
        self.tmp.cleanup()

    def package(self, sid, base="system", extra="", files=None):
        directory = self.root / sid
        (directory / "assets").mkdir(parents=True)
        (directory / "skin.toml").write_text(MANIFEST.format(id=sid, base=base, extra=extra))
        for name, content in (files or {"assets/bg.png": b"png'bytes"}).items():
            (directory / name).write_bytes(content)
        return directory

    def run_main(self, *args):
        out, err = io.StringIO(), io.StringIO()
        with contextlib.redirect_stdout(out), contextlib.redirect_stderr(err):
            code = seed.main([str(self.root), *args])
        return code, out.getvalue(), err.getvalue()

    def test_renders_one_idempotent_transaction(self):
        self.package("harbor", files={"assets/bg.png": b"png'bytes", "README.md": b"docs"})
        self.package("quay")
        code, sql, err = self.run_main()
        self.assertEqual(code, 0, err)
        lines = sql.splitlines()
        self.assertIn("-- Source: not a git checkout", lines)
        self.assertEqual(lines[2:5], ["BEGIN;", "SET LOCAL ROLE msime_backend;", "SET LOCAL lock_timeout = '5s';"])
        self.assertEqual(lines[-1], "COMMIT;")
        self.assertEqual(sql.count("BEGIN;"), 1)
        self.assertLess(sql.index("VALUES('harbor'"), sql.index("VALUES('quay'"))
        self.assertIn("decode('" + b"png'bytes".hex() + "','hex')", sql)
        self.assertIn(seed.sha256(b"png'bytes"), sql)
        self.assertIn("ON CONFLICT(id) DO NOTHING", sql)
        self.assertIn("RAISE EXCEPTION 'Candidate skin % differs", sql)
        self.assertNotIn("README.md", sql)
        self.assertIn("left out README.md", err)
        self.assertEqual(self.run_main()[1], sql, "output must be deterministic")
        self.assertIn("SET LOCAL ROLE app_rw;", self.run_main("--role", "app_rw")[1])
        with self.assertRaises(SystemExit), contextlib.redirect_stderr(io.StringIO()):
            seed.main([str(self.root), "--role", "x; DROP TABLE y"])

    def test_windows_looks_are_bases_and_other_names_are_refused(self):
        self.package("harbor", base="fluent")
        self.package("quay", base="wechat")
        code, sql, err = self.run_main()
        self.assertEqual(code, 0, err)
        # 清单按原样存储，只有服务端把 msime-windows 内置外观解析成 system。
        self.assertIn("base = 'fluent'".encode().hex(), sql)
        self.assertIn("base = 'wechat'".encode().hex(), sql)
        self.package("pier", base="sepia")
        code, sql, err = self.run_main()
        self.assertEqual((code, sql), (1, ""))
        self.assertIn("refused pier: base must be system or a built-in theme", err)
        self.assertIn("sepia", (self.root / "pier/skin.toml").read_text())

    def test_unverified_assets_are_skipped_or_refused(self):
        self.package("niya-demo", extra="[license]\nassets = 'UNVERIFIED-DEMO-ONLY'\n")
        self.package("harbor")
        code, sql, err = self.run_main()
        self.assertEqual(code, 0, err)
        self.assertIn("niya-demo: skipped", err)
        self.assertNotIn("niya-demo", sql)
        code, sql, err = self.run_main("--only", "niya-demo")
        self.assertEqual((code, sql), (1, ""))
        self.assertIn("refused niya-demo: license.assets is unverified", err)
        code, sql, err = self.run_main("--only", "harbor,missing")
        self.assertEqual(code, 1)
        self.assertIn("missing: no package directory", err)
        code, sql, _ = self.run_main("--only", "harbor")
        self.assertEqual(code, 0)

    def test_refuses_symlinks_oversized_and_empty_input(self):
        target = self.root.parent / (self.root.name + "-outside.png")
        target.write_bytes(b"secret")
        self.addCleanup(target.unlink)
        directory = self.package("harbor")
        os.symlink(target, directory / "assets/link.png")
        code, _, err = self.run_main()
        self.assertEqual(code, 1)
        self.assertIn("assets/link.png is a symbolic link", err)
        (directory / "assets/link.png").unlink()
        (directory / "assets/bg.png").write_bytes(b"x" * ((4 << 20) + 1))
        self.assertIn("exceeds 4 MiB", self.run_main()[2])
        (directory / "skin.toml").write_bytes(b"#" * 65537)
        self.assertIn("skin.toml is too large", self.run_main()[2])
        empty = tempfile.TemporaryDirectory()
        self.addCleanup(empty.cleanup)
        with contextlib.redirect_stderr(io.StringIO()) as err:
            self.assertEqual(seed.main([empty.name]), 1)
        self.assertIn("no packages to publish", err.getvalue())

    def test_git_provenance_records_commit_without_credentials(self):
        self.package("harbor")
        git = ["git", "-C", str(self.root), "-c", "user.name=t", "-c", "user.email=t@example.test"]
        for args in (["init", "-q"], ["remote", "add", "origin", "https://user:token@example.test/skins.git"], ["add", "."], ["commit", "-q", "-m", "fixture"]):
            subprocess.run(git + args, check=True, timeout=30, capture_output=True)
        commit = subprocess.run(git + ["rev-parse", "HEAD"], check=True, timeout=30, capture_output=True, text=True).stdout.strip()
        _, sql, _ = self.run_main()
        self.assertIn(f"-- Source: https://example.test/skins.git at commit {commit}", sql)
        self.assertNotIn("token", sql)
        self.assertNotIn("uncommitted", sql)
        (self.root / "harbor/assets/bg.png").write_bytes(b"changed")
        self.assertIn("WARNING: the checkout has uncommitted changes", self.run_main()[1])
        self.assertEqual(seed.sanitize_remote("git@example.test:org/skins.git"), "git@example.test:org/skins.git")


if __name__ == "__main__":
    unittest.main()
