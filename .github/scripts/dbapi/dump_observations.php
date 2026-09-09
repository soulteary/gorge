#!/usr/bin/env php
<?php

/**
 * Dump Phorge's own database observations to JSON, for the cross-repo test.
 *
 * This runs inside the Phorge container, on Phorge's real objects, so it is the
 * "at least one real PHP consumption (not a bare curl)" the integration test
 * requires. It reports what Phorge itself sees:
 *
 *   - `servers`: PhabricatorDatabaseRef::queryAll() connection/replica status;
 *   - `schema`: PhabricatorConfigSchemaQuery::loadActualSchemata() flattened to
 *     database/table/column/index attributes;
 *   - `setupIssues`: the DB/MySQL setup checks' issue keys and fatality.
 *
 * The trick is that this script does NOT know or care which path produced the
 * data. When `gorge.db.uri` is unset, every call above runs Phorge's native
 * direct-SQL reflection; when it is set to the service, the very same calls
 * read gorge-db-api through this repo's PHP adapter. The workflow runs this
 * script once each way against one database and diffs the two dumps with
 * scripts/dbapi/compare_native_vs_gorge.py, so a consumer that drops a field or
 * reads the wrong key makes the two disagree.
 *
 * The path is resolved relative to a Phorge checkout; the workflow copies this
 * file into `phorge/scripts/dbapi/` before running it so `__init_script__.php`
 * is three directories up, matching every other script in that tree.
 *
 * Usage (inside the Phorge container):
 *   php scripts/dbapi/dump_observations.php > out.json
 */

$root = dirname(dirname(dirname(__FILE__)));
require_once $root.'/scripts/__init_script__.php';

function gorge_bool($value) {
  if ($value === null) {
    return null;
  }
  return (bool)$value;
}

$out = array(
  'servers' => array(),
  'schema' => array(),
  'setupIssues' => array(),
);

// --- servers -------------------------------------------------------------

$refs = PhabricatorDatabaseRef::queryAll();
foreach ($refs as $ref) {
  $out['servers'][] = array(
    'refKey' => $ref->getRefKey(),
    'isMaster' => gorge_bool($ref->getIsMaster()),
    'connectionStatus' => $ref->getConnectionStatus(),
    'replicationStatus' => $ref->getReplicaStatus(),
    // Volatile: compared by type/range only, never for equality.
    'connectionLatencySec' => $ref->getConnectionLatency(),
    'secondsBehindMaster' => $ref->getReplicaDelay(),
  );
}

// --- schema --------------------------------------------------------------

$query = id(new PhabricatorConfigSchemaQuery());
$schemata = $query->loadActualSchemata();

foreach ($schemata as $ref_key => $server_schema) {
  $server_dump = array(
    'refKey' => $ref_key,
    'databases' => array(),
  );

  foreach ($server_schema->getDatabases() as $database) {
    $db_dump = array(
      'name' => $database->getName(),
      'characterSet' => $database->getCharacterSet(),
      'collation' => $database->getCollation(),
      'accessDenied' => gorge_bool($database->getAccessDenied()),
      'tables' => array(),
    );

    foreach ($database->getTables() as $table) {
      $table_dump = array(
        'name' => $table->getName(),
        'engine' => $table->getEngine(),
        'collation' => $table->getCollation(),
        'columns' => array(),
        'keys' => array(),
      );

      foreach ($table->getColumns() as $column) {
        $table_dump['columns'][] = array(
          'name' => $column->getName(),
          'columnType' => $column->getColumnType(),
          'characterSet' => $column->getCharacterSet(),
          'collation' => $column->getCollation(),
          'nullable' => gorge_bool($column->getNullable()),
          'autoIncrement' => gorge_bool($column->getAutoIncrement()),
        );
      }

      foreach ($table->getKeys() as $key) {
        $table_dump['keys'][] = array(
          'name' => $key->getName(),
          'columnNames' => $key->getColumnNames(),
          'unique' => gorge_bool($key->getUnique()),
          'indexType' => $key->getIndexType(),
        );
      }

      $db_dump['tables'][] = $table_dump;
    }

    $server_dump['databases'][] = $db_dump;
  }

  $out['schema'][] = $server_dump;
}

// --- setup issues --------------------------------------------------------

$checks = array(
  new PhabricatorDatabaseSetupCheck(),
  new PhabricatorMySQLSetupCheck(),
);

foreach ($checks as $check) {
  $check->runSetupChecks();
  foreach ($check->getIssues() as $issue) {
    $out['setupIssues'][] = array(
      'issueKey' => $issue->getIssueKey(),
      'isFatal' => gorge_bool($issue->getIsFatal()),
    );
  }
}

echo id(new PhutilJSON())->encodeFormatted($out);
