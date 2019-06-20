/*
 * Copyright (c) 2013-2019, Jeremy Bingham (<jeremy@goiardi.gl>)
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package client

import (
	"database/sql"
	"errors"
	"fmt"
	"github.com/ctdk/goiardi/config"
	"github.com/ctdk/goiardi/datastore"
	"github.com/ctdk/goiardi/organization"
	"github.com/ctdk/goiardi/util"
	"log"
	"net/http"
	"strings"
)

func checkForClientSQL(dbhandle datastore.Dbhandle, org *organization.Organization, name string) (bool, error) {
	_, err := datastore.CheckForOne(dbhandle, "clients", org.GetId(), name)
	if err == nil {
		return true, nil
	}
	if err != sql.ErrNoRows {
		return false, err
	}
	return false, nil
}

// TODO: Don't fill the client Orgname like this
func (c *Client) fillClientFromSQL(row datastore.ResRow) error {
	err := row.Scan(&c.Name, &c.NodeName, &c.Validator, &c.Admin, &c.pubKey, &c.Certificate, &c.id)
	if err != nil {
		return err
	}
	c.ChefType = "client"
	c.JSONClass = "Chef::ApiClient"
	return nil
}

func getClientSQL(name string, org *organization.Organization) (*Client, error) {
	client := new(Client)
	client.org = org

	sqlStatement := "select name, nodename, validator, admin, public_key, certificate, id FROM goiardi.clients WHERE organization_id = $1 AND name = $2"

	stmt, err := datastore.Dbh.Prepare(sqlStatement)
	if err != nil {
		return nil, err
	}
	defer stmt.Close()
	row := stmt.QueryRow(org.GetId(), name)
	err = client.fillClientFromSQL(row)
	if err != nil {
		return nil, err
	}
	return client, nil
}

func getMultiSQL(clientNames []string, org *organization.Organization) ([]*Client, error) {
	bind := make([]string, len(clientNames))

	for i := range clientNames {
		bind[i] = fmt.Sprintf("$%d", i+2)
	}
	sqlStmt = fmt.Sprintf("select name, nodename, validator, admin, public_key, certificate FROM goiardi.clients WHERE organization_id = $1 AND name IN (%s)", strings.Join(bind, ", "))
	stmt, err := datastore.Dbh.Prepare(sqlStmt)
	if err != nil {
		return nil, err
	}
	defer stmt.Close()
	nameArgs := make([]interface{}, len(clientNames))
	for i, v := range clientNames {
		nameArgs[i] = v
	}
	rows, err := stmt.Query(org.GetId(), nameArgs...)
	if err != nil {
		return nil, err
	}
	clients := make([]*Client, 0, len(clientNames))
	for rows.Next() {
		c := new(Client)
		err = c.fillClientFromSQL(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		clients = append(clients, c)
	}

	rows.Close()
	if err = rows.Err(); err != nil {
		return nil, err
	}
	return clients, nil
}

func (c *Client) deleteSQL() error {
	tx, err := datastore.Dbh.Begin()
	if err != nil {
		gerr := util.CastErr(err)
		gerr.SetStatus(http.StatusInternalServerError)
		return gerr
	}

	_, err = tx.Exec("DELETE FROM goiardi.clients WHERE organization_id = $1 AND name = $2", c.org.GetId(), c.Name)

	if err != nil {
		tx.Rollback()
		gerr := util.CastErr(err)
		gerr.SetStatus(http.StatusInternalServerError)
		return gerr
	}
	tx.Commit()
	return nil
}

// This may be hopelessly obsolete with the new RBAC stuff.
func numAdminsSQL(org *organization.Organization) int {
	var numAdmins int

	sqlStatement := "SELECT count(*) FROM goiardi.clients WHERE organization_id = $1 AND admin = TRUE"
	stmt, err := datastore.Dbh.Prepare(sqlStatement)
	if err != nil {
		log.Fatal(err)
	}
	defer stmt.Close()
	err = stmt.QueryRow(org.GetId()).Scan(&numAdmins)
	if err != nil {
		log.Fatal(err)
	}
	return numAdmins
}

func getListSQL(org *organization.Organization) []string {
	var clientList []string
	var sqlStatement string
	if config.Config.UseMySQL {
		sqlStatement = "SELECT name FROM clients"
	} else if config.Config.UsePostgreSQL {
		sqlStatement = "SELECT name FROM goiardi.clients"
	}
	rows, err := datastore.Dbh.Query(sqlStatement)
	if err != nil {
		if err != sql.ErrNoRows {
			log.Fatal(err)
		}
		rows.Close()
		return clientList
	}
	for rows.Next() {
		var clientName string
		err = rows.Scan(&clientName)
		if err != nil {
			log.Fatal(err)
		}
		clientList = append(clientList, clientName)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		log.Fatal(err)
	}
	return clientList
}
func allClientsSQL(org *organization.Organization) []*Client {
	var clients []*Client
	var sqlStatement string
	if config.Config.UseMySQL {
		sqlStatement = "SELECT c.name, nodename, validator, admin, o.name, public_key, certificate FROM clients c JOIN organizations o ON c.organization_id = o.id"
	} else if config.Config.UsePostgreSQL {
		sqlStatement = "SELECT c.name, nodename, validator, admin, o.name, public_key, certificate FROM goiardi.clients c JOIN goiardi.organizations o ON c.organization_id = o.id"
	}

	stmt, err := datastore.Dbh.Prepare(sqlStatement)
	if err != nil {
		log.Fatal(err)
	}
	defer stmt.Close()
	rows, qerr := stmt.Query()
	if qerr != nil {
		if qerr == sql.ErrNoRows {
			return clients
		}
		log.Fatal(qerr)
	}
	for rows.Next() {
		cl := new(Client)
		err = cl.fillClientFromSQL(rows)
		if err != nil {
			log.Fatal(err)
		}
		clients = append(clients, cl)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		log.Fatal(err)
	}
	return clients
}

func ClientsByIdSQL(ids []int64, org *organization.Organization) ([]*Client, error) {
	if !config.UsingDB() {
		return nil, errors.New("ClientsByIdSQL only works if you're using a database storage backend.")
	}

	var clients []*Client

	bind := make([]string, len(ids))
	intfIds := make([]interface{}, len(ids))

	for i, d := range ids {
		bind[i] = fmt.Sprintf("$%d", i + 1)
		intfIds[i] = d
	}
	sqlStatement := fmt.Sprintf("select c.name, nodename, validator, admin, o.name, public_key, certificate, id FROM goiardi.clients c JOIN goiardi.organizations o on c.organization_id = o.id WHERE id IN (%s)", strings.Join(bind, ", "))

	stmt, err := datastore.Dbh.Prepare(sqlStatement)
	if err != nil {
		return nil, err
	}
	defer stmt.Close()
	rows, qerr := stmt.Query(intfIds...)
	if qerr != nil {
		if qerr == sql.ErrNoRows {
			return clients, nil
		}
		return nil, qerr
	}
	for rows.Next() {
		cs := new(Client)
		err = cs.fillClientFromSQL(rows)
		if err != nil {
			return nil, err
		}
		clients = append(clients, cs)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return nil, err
	}
	return clients, nil
}
