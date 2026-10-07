import importlib.util
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

sys.dont_write_bytecode = True
spec = importlib.util.spec_from_file_location("upgrade_history", Path(__file__).with_name("upgrade-history.py"))
history = importlib.util.module_from_spec(spec)
spec.loader.exec_module(history)


class UpgradeHistoryTest(unittest.TestCase):
    def test_keep_three_changed_tags_and_ignore_development_steps(self):
        versions = [("v0.3.2", 34), ("v0.3.3", 34), ("v0.4.0", 36),
                    ("v0.4.1", 36), ("v0.5.0", 38), ("v0.6.0", 39)]
        result = history.retain_updates(33, versions, 41)
        self.assertEqual(result, dict(base_schema=34, updates=[
            {"tag": "v0.4.0", "from": 34, "to": 36},
            {"tag": "v0.5.0", "from": 36, "to": 38},
            {"tag": "v0.6.0", "from": 38, "to": 39},
        ]))
        # A tag with no schema change and additional untagged schemas consume no slots.
        self.assertEqual(history.retain_updates(33, versions + [("v0.6.1", 39)], 42), result)
        tagged = history.retain_updates(33, versions + [("v0.7.0", 41)], 41)
        self.assertEqual(tagged["base_schema"], 36)
        self.assertEqual(tagged["updates"][-1], {"tag": "v0.7.0", "from": 39, "to": 41})

    def test_no_changes_and_regressions(self):
        self.assertEqual(history.retain_updates(33, [("v0.3.2", 33)], 35),
                         {"base_schema": 33, "updates": []})
        with self.assertRaises(ValueError):
            history.retain_updates(33, [("v0.3.2", 32)], 34)
        with self.assertRaises(ValueError):
            history.retain_updates(33, [("v0.3.2", 35)], 34)

    def test_real_tags_only_and_release_validation(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            def git(*args):
                return history.git(root, *args)
            git("init", "-q")
            git("config", "user.name", "Upgrade history test")
            git("config", "user.email", "test@example.invalid")
            git("config", "commit.gpgsign", "false")
            git("config", "tag.gpgsign", "false")
            path = root / history.SCHEMA_PATH
            path.parent.mkdir(parents=True)
            def commit(version):
                path.write_text(f"package platform\nconst DatabaseVersion = {version}\n")
                git("add", ".")
                git("commit", "--allow-empty", "-qm", f"schema {version}")
            commit(33)
            git("tag", "v0.3.1")
            commit(34)
            commit(35)
            self.assertEqual(history.generate(root), {"base_schema": 33, "updates": []})
            git("tag", "-am", "first changed tag", "v0.4.0")
            result = history.generate(root, "v0.4.0")
            self.assertEqual(result["updates"], [{"tag": "v0.4.0", "from": 33, "to": 35}])
            commit(35)
            git("tag", "v0.4.1")
            self.assertEqual(history.generate(root, "v0.4.1"), result)
            with self.assertRaises(ValueError):
                history.generate(root, "v0.4.0")  # Actual tag, but not HEAD.
            commit(36)
            self.assertEqual(history.generate(root), result)
            with self.assertRaises(subprocess.CalledProcessError):
                history.generate(root, "v0.4.2")  # A version string isn't a tag.
            # A shallow checkout must fail instead of silently forgetting history.
            shallow = root / "shallow"
            git("clone", "--quiet", "--depth=1", root.as_uri(), str(shallow))
            with self.assertRaises(ValueError):
                history.generate(shallow)


if __name__ == "__main__":
    unittest.main()
