import importlib.util
from pathlib import Path
import unittest
spec = importlib.util.spec_from_file_location("checker", Path(__file__).with_name("dbapi-response-check.py"))
checker = importlib.util.module_from_spec(spec)
spec.loader.exec_module(checker)
class Responses(unittest.TestCase):
    def test_public_identifiers(self):
        self.assertTrue(checker.safe({"data": [{"user": "shared", "tableName": "patch_status", "databaseName": "shared_meta"}]}, "shared"))
    def test_credentials_and_sql(self):
        for body in [{"password": "hidden"}, {"data": {"DSN": "hidden"}}, {"error": {"message": "password shared"}}, {"error": {"message": "SELECT * FROM patch_status"}}]:
            self.assertFalse(checker.safe(body, "shared"))
    def test_nested(self):
        self.assertFalse(checker.safe({"data": [{"details": {"token": "hidden"}}]}))
if __name__ == "__main__": unittest.main()
