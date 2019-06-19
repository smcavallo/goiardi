/*
 * Copyright (c) 2013-2019, Jeremy Bingham (<jbingham@gmail.com>)
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

package group

// SQL goodies for groups

import (
	"database/sql"
	"errors"
	"fmt"
	"github.com/ctdk/goiardi/actor"
	"github.com/ctdk/goiardi/client"
	"github.com/ctdk/goiardi/config"
	"github.com/ctdk/goiardi/datastore"
	"github.com/ctdk/goiardi/organization"
	"github.com/ctdk/goiardi/user"
)

func checkForGroupSQL(dbhandle datastore.Dbhandle, org *organization.Organization, name string) (bool, error) {
	_, err := datastore.CheckForOne(dbhandle, "groups", org.GetId(), name)
	if err == nil {
		return true, nil
	}
	if err != sql.ErrNoRows {
		return false, err
	}
	return false, nil
}

func (g *Group) fillGroupFromSQL(row datastore.ResRow) error {
	var userIds []int64
	var clientIds []int64
	var groupIds []int64
	var orgId int64
	
	// arrrgh blargh, it looks like we may also need to create a special
	// type for getting the arrays of ints out of postgres.

	// eeesh, there isn't a whole lot we can fill in directly.
	err := row.Scan(&g.Name, &orgId, &userIds, &clientIds, &groupIds)
	if err != nil {
		return err
	}

	// Perform a quick sanity check because why not
	if orgId != g.Org.GetId() {
		return fmt.Errorf("org id %d returned from query somehow did not match the expected id %d for %s", orgId, g.Org.GetId(), g.Org.Name)
	}
	/*
	 * Only fill in the child actors & groups if it's the main group we're
	 * interested in. Otherwise, skip over this. On the off chance we ever
	 * need the grandchild groups, we can reload the group in question. If
	 * the 'getChildren' flag in a group object is false, its members have
	 * not been loaded yet.
	 *
	 * NOTE: this is in lieu of retrieving the whole tree of groups & their
	 * members, both to avoid needlessly large data structures and the time
	 * spent processing the queries to get them, but also to avoid getting
	 * stuck in a loop. Should this not be sufficient, it'll need to be
	 * dealt with more thoroughly.
	 */

	if g.getChildren {
		// fill in the actor and group slices with the appropriate
		// objects. Will these need to be sorted? We'll see.

		groupez, err := GroupsByIdSQL(groupIds)
		if err != nil {
			return err
		}
		g.Groups = groupez

		userez, err := user.UsersByIdSQL(userIds)
		if err != nil {
			return err
		}

		clientez, err := client.ClientsByIdSQL(clientIds)
		if err != nil {
			return nil
		}

		actorez := make([]actor.Actor, 0, len(userez) + len(clientez))
		// may need to do the explicit for range loop.
		actorez = append(actorez, userez...)
		actorez = append(actorez, clientez...)
		g.Actors = actorez
	}

	return nil
}

func getGroupSQL(name string, org *organization.Organization) (*Group, error) {
	var sqlStatement string
	g := new(Group)
	g.Org = org

	if config.Config.UseMySQL {
		// MySQL will be rather more intricate than postgres, I'm
		// afraid. Leaving this here for now.
		sqlStatement = "SELECT name, organization_id FROM groups WHERE name = ?"
	} else if config.Config.UsePostgreSQL {
		// bleh, break this apart into multiple lines so there's some
		// small hope of reading and understanding it later.
		sqlStatement = `select name, organization_id, u.user_ids, c.client_ids, mg.group_ids FROM goiardi.groups g
		LEFT JOIN 
			(SELECT gau.group_id AS ugid, ARRAY_AGG(gau.user_id) AS user_ids FROM goiardi.group_actor_users gau JOIN goiardi.groups gs ON gs.id = gau.group_id group by gau.group_id) u ON u.ugid = groups.id 
		LEFT JOIN 
			(SELECT gac.group_id AS cgid, ARRAY_AGG(gac.client_id) AS client_ids FROM goiardi.group_actor_clients gac JOIN goiardi.groups gs ON gs.id = gac.group_id group by gac.group_id) c ON c.cgid = groups.id
		LEFT JOIN 
			(SELECT gg.group_id AS ggid, ARRAY_AGG(gg.member_group_id) AS group_ids FROM goiardi.group_groups gg JOIN goiardi.groups gs ON gs.id = gg.group_id group by gg.group_id) mg ON mg.ggid = groups.id
		WHERE organization_id = $1 AND name = $2`
	}

	stmt, err := datastore.Dbh.Prepare(sqlStatement)
	if err != nil {
		return nil, err
	}
	defer stmt.Close()

	row := stmt.QueryRow(org.GetId(), name);

	g.getChildren = true
	if err = g.fillGroupFromSQL(row); err != nil {
		return nil, err
	}
	return g, nil
}

func (g *Group) saveSQL() error {
	// deal with mysql later, if at all. If we don't, of course, the
	// contents of savePostgreSQL() should move into here.
	//
	// Reminder: the SQL save methods will also need to deal with saving
	// member actors and groups.

	// get arrays of ids for saving
	user_ids := make([]int64, 0)
	client_ids := make([]int64, 0)
	group_ids := make([]int64, len(g.Groups))

	// get the groups out of the way
	for i, mg := range g.Groups {
		group_ids[i] = mg.GetId()
	}

	// and actors
	for _, act := range g.Actors() {
		if act.IsUser() {
			user_ids = append(user_ids, act.GetId())
		} else {
			client_ids = append(client_ids, act.GetId())
		}
	}

	return g.savePostgreSQL(user_ids, client_ids, group_ids)
}

// The Add/Del Actor/Group methods don't need SQL methods, so they're left out
// in here.

func (g *Group) renameSQL(newName string) error {
	tx, err := datastore.Dbh.Begin()
	if err != nil {
		gerr := util.Errorf(err.Error())
		return gerr
	}
	_, err = tx.Exec("SELECT goiardi.rename_group($1, $2)", g.Name, newName)
	if err != nil {
		tx.Rollback()
		gerr := util.Errorf(err.Error())
		if strings.HasPrefix(err.Error(), strings.Contains(err.Error(), "already exists, cannot rename")) {
			gerr.SetStatus(http.StatusConflict)
		} else {
			gerr.SetStatus(http.StatusInternalServerError)
		}
		return gerr
	}
	g.Name = newName
	tx.Commit()
	return nil
}

func (g *Group) deleteSQL() error {
	tx, err := datastore.Dbh.Begin()
	if err != nil {
		return err
	}

	// Live dangerously, use foreign keys w/ ON DELETE CASCADE to clear out
	// the associations.

	sqlStmt := "DELETE FROM goiardi.groups WHERE id = $1"
	_, err = tx.Exec(sqlStmt, g.GetId())
	if err != nil {
		terr := tx.Rollback()
		if terr != nil {
			err = fmt.Errorf("deleting group %s from organization %s had an error '%s', and then rolling back the transaction gave another error '%s'", g.Name, err.Error(), terr.Error())
		}
		return err
	}
	tx.Commit()
	return nil
}

func getListSQL(org *organization.Organization) ([]string, error) {
	var groupList []string

	sqlStatement := "SELECT name FROM goiardi.groups WHERE organization_id = $1"
	stmt, err := datastore.Dbh.Prepare(sqlStatement)
	if err != nil {
		return nil, err
	}
	defer stmt.Close()
	rows, qerr := stmt.Query(org.GetId())
	if qerr != nil {
		if qerr == sql.ErrNoRows {
			return users, nil
		}
		return nil, qerr
	}
	for rows.Next() {
		var gName string
		err := row.Scan(&gName)
		if err != nil {
			return nil, err
		}
		groupList = append(groupList, gName)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return nil, err
	}
	return groupList, nil
}

func allGroupsSQL(org *organization.Organization) ([]*Group, error) {
	if !config.UsingDB() {
		return nil, errors.New("allGroupsSQL only works if you're using a database storage backend.")
	}

	var groups []*Groups
	var sqlStatement string

	if config.Config.UseMySQL {
		return nil, errors.New("Groups are not implemented with the MySQL backend yet, punting for now.")
	} else if config.Config.UsePostgreSQL {
		sqlStatement = `select name, organization_id, u.user_ids, c.client_ids, mg.group_ids FROM goiardi.groups g
		LEFT JOIN 
			(SELECT gau.group_id AS ugid, ARRAY_AGG(gau.user_id) AS user_ids FROM goiardi.group_actor_users gau JOIN goiardi.groups gs ON gs.id = gau.group_id group by gau.group_id) u ON u.ugid = groups.id 
		LEFT JOIN 
			(SELECT gac.group_id AS cgid, ARRAY_AGG(gac.client_id) AS client_ids FROM goiardi.group_actor_clients gac JOIN goiardi.groups gs ON gs.id = gac.group_id group by gac.group_id) c ON c.cgid = groups.id
		LEFT JOIN 
			(SELECT gg.group_id AS ggid, ARRAY_AGG(gg.member_group_id) AS group_ids FROM goiardi.group_groups gg JOIN goiardi.groups gs ON gs.id = gg.group_id group by gg.group_id) mg ON mg.ggid = groups.id
		WHERE g.organization_id = $1`
	}

	stmt, err := datastore.Dbh.Prepare(sqlStatement)
	if err != nil {
		return nil, err
	}
	defer stmt.Close()
	rows, qerr := stmt.Query(org.GetId())
	if qerr != nil {
		if qerr == sql.ErrNoRows {
			return users, nil
		}
		return nil, qerr
	}
	for rows.Next() {
		g := new(Group)
		g.Org = org
		err = g.fillGroupFromSQL(rows)
		if err != nil {
			return nil, err
		}
		groups = append(groups, g)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return nil, err
	}
	return groups, nil
}

func clearActorSQL(org *organization.Organization, act actor.Actor) error {
	tx, err := datastore.Dbh.Begin()
	if err != nil {
		return err
	}

	var actType string
	if act.IsUser() {
		actType = "user"
	} else {
		actType = "client"
	}

	sqlStmt := fmt.Sprintf("DELETE FROM goiardi.group_actor_%ss WHERE organization_id = $1 AND %s_id = $1")

	_, err = tx.Exec(sqlStmt, act.GetName(), g.Org.GetId())
	if err != nil {
		terr := tx.Rollback()
		if terr != nil {
			err = fmt.Errorf("clearing actor %s from organization %s had an error '%s', and then rolling back the transaction gave another error '%s'", act.GetName(), g.Org.Name, err.Error(), terr.Error())
		}
		return err
	}
	tx.Commit()
	return nil
}

func GroupsByIdSQL(ids []int64) ([]*Group, error) {
	if !config.UsingDB() {
		return nil, errors.New("GroupsByIdSQL only works if you're using a database storage backend.")
	}

	var groups []*Groups
	var sqlStatement string

	bind := make([]string, len(ids))
	intfIds := make([]interface{}, len(ids))

	if config.Config.UseMySQL {
		return nil, errors.New("Groups are not implemented with the MySQL backend yet, punting for now.")
	} else if config.Config.UsePostgreSQL {
		for i, d := range ids {
			bind[i] = fmt.Sprintf("$%d", i + 1)
			intfIds[i] = d
		}
		sqlStatement = fmt.Sprintf(`select name, organization_id, u.user_ids, c.client_ids, mg.group_ids FROM goiardi.groups g
		LEFT JOIN 
			(SELECT gau.group_id AS ugid, ARRAY_AGG(gau.user_id) AS user_ids FROM goiardi.group_actor_users gau JOIN goiardi.groups gs ON gs.id = gau.group_id group by gau.group_id) u ON u.ugid = groups.id 
		LEFT JOIN 
			(SELECT gac.group_id AS cgid, ARRAY_AGG(gac.client_id) AS client_ids FROM goiardi.group_actor_clients gac JOIN goiardi.groups gs ON gs.id = gac.group_id group by gac.group_id) c ON c.cgid = groups.id
		LEFT JOIN 
			(SELECT gg.group_id AS ggid, ARRAY_AGG(gg.member_group_id) AS group_ids FROM goiardi.group_groups gg JOIN goiardi.groups gs ON gs.id = gg.group_id group by gg.group_id) mg ON mg.ggid = groups.id
		WHERE id in (%s)`, strings.Join(bind, ", "))
	}

	stmt, err := datastore.Dbh.Prepare(sqlStatement)
	if err != nil {
		return nil, err
	}
	defer stmt.Close()
	rows, qerr := stmt.Query(intfIds...)
	if qerr != nil {
		if qerr == sql.ErrNoRows {
			return users, nil
		}
		return nil, qerr
	}
	for rows.Next() {
		mg := new(Group)
		err = mg.fillGroupFromSQL(rows)
		if err != nil {
			return nil, err
		}
		groups = append(groups, mg)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return nil, err
	}
	return groups, nil
}
