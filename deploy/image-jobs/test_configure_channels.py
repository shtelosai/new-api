"""保障渠道配置默认只读、重复运行幂等、同名异配不覆盖旧数据。"""
import importlib.util
from pathlib import Path
import sqlite3
import unittest

spec = importlib.util.spec_from_file_location('configure_channels', Path(__file__).with_name('configure_channels.py'))
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)


class ConfigureChannelsTest(unittest.TestCase):
    def setUp(self):
        self.db = sqlite3.connect(':memory:')
        self.addCleanup(self.db.close)
        self.db.executescript('''
            CREATE TABLE channels (id INTEGER PRIMARY KEY, name TEXT, type INTEGER,
                key TEXT, status INTEGER, base_url TEXT, models TEXT, `group` TEXT,
                priority INTEGER, weight INTEGER, tag TEXT, setting TEXT);
            CREATE TABLE abilities (`group` TEXT, model TEXT, channel_id INTEGER,
                enabled INTEGER, priority INTEGER, weight INTEGER, tag TEXT,
                PRIMARY KEY (`group`,model,channel_id));
            INSERT INTO channels(id,name,models,priority) VALUES(198,'old-image','gpt-image-2.5-flare',110);
        ''')
        self.specs = module.channel_specs('http://127.0.0.1:8090/kie',
            'http://127.0.0.1:8090/apimart', 'local-fixture-key-' * 3, 'default')

    def run_config(self, **options):
        return module.configure(self.db, self.specs, placeholder='?', **options)

    def test_dry_run_does_not_write(self):
        changes = self.db.total_changes
        summary = self.run_config()
        self.assertEqual(summary['mode'], 'dry-run')
        self.assertEqual(len(summary['channels']), 4)
        self.assertEqual(self.db.total_changes, changes)
        self.assertNotIn(self.specs[0]['key'], str(summary))

    def test_apply_is_idempotent_and_preserves_old_channel(self):
        first = self.run_config(apply=True)
        changes = self.db.total_changes
        second = self.run_config(apply=True)
        self.assertEqual(self.db.total_changes, changes)
        self.assertEqual([x['id'] for x in first['channels']], [x['id'] for x in second['channels']])
        self.assertEqual(self.db.execute('SELECT priority FROM channels WHERE id=198').fetchone(), (110,))
        self.assertEqual(self.db.execute('SELECT COUNT(*) FROM abilities').fetchone(), (4,))
        self.assertEqual(self.db.execute('SELECT DISTINCT tag FROM abilities').fetchall(), [('image-async',)])

    def test_conflicting_existing_channel_prevents_all_mutations(self):
        self.run_config(apply=True)
        self.db.execute('UPDATE channels SET priority=1 WHERE name=?', (self.specs[-1]['name'],))
        self.db.commit()
        changes = self.db.total_changes
        with self.assertRaises(ValueError):
            self.run_config(apply=True)
        self.assertEqual(self.db.total_changes, changes)


if __name__ == '__main__':
    unittest.main()
