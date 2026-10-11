import unittest
from check_go_coverage import coverage, check, check_database


class CoverageTests(unittest.TestCase):
    def test_duplicate_blocks_union_hits(self):
        stats = coverage("mode: atomic\nm/a.go:1.1,2.1 9 0\nm/a.go:1.1,2.1 9 3\nm/a.go:3.1,4.1 1 0\n")
        self.assertEqual(stats["total"], [10, 9])
        self.assertEqual(stats["m"], [10, 9])

    def test_invalid_profiles(self):
        for profile in ("", "mode: atomic\n", "mode: bad", "mode: set\nbad", "mode: set\nm/a.go:1.1,2.1 1 -1", "mode: set\nm/a.go:1.1,2.1 1 1\nm/a.go:1.1,2.1 2 1"):
            with self.subTest(profile=profile), self.assertRaises(ValueError):
                coverage(profile)

    def test_scope_and_exact_threshold(self):
        module = "github.com/metasequoiaime/MSIME-Backend"
        packages = [module + suffix for suffix in ("", "/cmd/msime-server", "/cmd/msime-cloud", "/admin-web", "/internal/account", "/internal/server", "/internal/engine", "/internal/skins")]
        stats = dict.fromkeys(packages + ["total"], (100, 90))
        check(stats)
        for scope in ("total", module + "/internal/account", module + "/internal/server"):
            with self.subTest(scope=scope), self.assertRaises(ValueError):
                check(dict(stats, **{scope: (10000, 8999)}))
        with self.assertRaises(ValueError):
            check(dict(stats, **{module + "/internal/account": (0, 0)}))
        del stats[module + "/cmd/msime-server"]
        with self.assertRaises(ValueError):
            check(stats)

    def test_database_scope_checks_only_account_and_server(self):
        module = "github.com/metasequoiaime/MSIME-Backend"
        account, server = module + "/internal/account", module + "/internal/server"
        stats = {"total": (100, 10), account: (100, 85), server: (100, 88)}
        check_database(stats)
        for scope, failing in ((account, (10000, 8499)), (server, (10000, 8799))):
            with self.subTest(scope=scope), self.assertRaises(ValueError):
                check_database(dict(stats, **{scope: failing}))
        for scope in (account, server):
            with self.subTest(missing=scope), self.assertRaises(ValueError):
                check_database({key: value for key, value in stats.items() if key != scope})


if __name__ == "__main__":
    unittest.main()
