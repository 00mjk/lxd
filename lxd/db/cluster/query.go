package cluster

import (
	"database/sql"
	"fmt"

	"github.com/lxc/lxd/lxd/db/query"
)

// Update the schema and api_extensions columns of the row in the nodes table
// that matches the given id.
//
// If not such row is found, an error is returned.
func updateNodeVersion(tx *sql.Tx, address string, apiExtensions int) error {
	stmt := "UPDATE nodes SET schema=?, api_extensions=? WHERE address=?"
	result, err := tx.Exec(stmt, len(updates), apiExtensions, address)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("updated %d rows instead of 1", n)
	}
	return nil
}

// Return the number of rows in the nodes table that have their address column
// set to '0.0.0.0'.
func selectUnclusteredNodesCount(tx *sql.Tx) (int, error) {
	return query.Count(tx, "nodes", "address='0.0.0.0'")
}

// Return a slice of binary integer tuples. Each tuple contains the schema
// version and number of api extensions of a node in the cluster.
func selectNodesVersions(tx *sql.Tx) ([][2]int, error) {
	versions := [][2]int{}

	dest := func(i int) []any {
		versions = append(versions, [2]int{})
		return []any{&versions[i][0], &versions[i][1]}
	}

	stmt, err := tx.Prepare("SELECT schema, api_extensions FROM nodes WHERE state=0")
	if err != nil {
		// In order to make cluster updates work, let's check for "pending" as well as that's the column's previous name.
		stmt, err = tx.Prepare("SELECT schema, api_extensions FROM nodes WHERE pending=0")
		if err != nil {
			return nil, err
		}
	}
	defer func() { _ = stmt.Close() }()
	err = query.SelectObjects(stmt, dest)
	if err != nil {
		return nil, err
	}
	return versions, nil
}
