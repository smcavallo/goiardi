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

package acl

import (
	"fmt"
	"github.com/casbin/casbin"
	"github.com/casbin/casbin/model"
	"github.com/casbin/casbin/persist"
	"github.com/casbin/casbin/persist/file-adapter"
	"github.com/ctdk/goiardi/aclhelper"
	"github.com/ctdk/goiardi/actor"
	"github.com/ctdk/goiardi/config"
	"github.com/ctdk/goiardi/container"
	"github.com/ctdk/goiardi/datastore"
	"github.com/ctdk/goiardi/group"
	"github.com/ctdk/goiardi/organization"
	"github.com/ctdk/goiardi/util"
	"github.com/tideland/golib/logger"
	"net/http"
	"os"
	"path"
	"strings"
	"sync"
)

type enforceCondition []interface{}

type Checker struct {
	org *organization.Organization
	e   *casbin.SyncedEnforcer
	// gah, take a mutex to keep these perms from overwriting each other
	m             sync.RWMutex
	inTransaction bool
}

// group, subkind, kind, name, perm, effect
const (
	condGroupPos = iota
	condSubkindPos
	condKindPos
	condNamePos
	condPermPos
	condEffectPos
)

const (
	enforceEffect = "allow"
	denyEffect    = "deny"
	policyFileFmt = "%s-policy.csv"
	addPerm       = "add"
	removePerm    = "remove"
)

// Bleh. Do what we must, I guess.
// Eventually this will likely need to have separate coordinating chans per
// organization, but not today.
var ACLCoordinator chan struct{}

var DefaultUser = "pivotal" // should this be configurable?

func init() {
	ACLCoordinator = make(chan struct{}, 1)
}

func LoadACL(org *organization.Organization) error {
	m := casbin.NewModel(modelDefinition)
	if !policyExists(org, config.Config.PolicyRoot) {
		newE, err := initializeACL(org, m)
		if err != nil {
			return err
		}
		c := &Checker{org: org, e: newE}
		org.PermCheck = c
		return nil
	}
	pa, err := loadPolicyAdapter(org)
	if err != nil {
		return err
	}
	e := casbin.NewSyncedEnforcer(m, pa, config.Config.PolicyLogging)
	e.EnableAutoSave(true)
	c := &Checker{org: org, e: e, inTransaction: false}
	org.PermCheck = c

	return nil
}

func initializeACL(org *organization.Organization, m model.Model) (*casbin.SyncedEnforcer, error) {
	if err := initializePolicy(org, config.Config.PolicyRoot); err != nil {
		return nil, err
	}
	adp, err := loadPolicyAdapter(org)
	if err != nil {
		return nil, err
	}
	e := casbin.NewSyncedEnforcer(m, adp, config.Config.PolicyLogging)

	return e, nil
}

// TODO: When 1.0.0-dev starts wiring in the DBs, set up DB adapters for
// policies. Until that time, set up a file backed one.
func loadPolicyAdapter(org *organization.Organization) (persist.Adapter, error) {
	if config.UsingDB() {

	}
	return loadPolicyFileAdapter(org, config.Config.PolicyRoot)
}

func loadPolicyFileAdapter(org *organization.Organization, policyRoot string) (persist.Adapter, error) {
	if !policyExists(org, policyRoot) {
		err := fmt.Errorf("Cannot load ACL policy for organization %s: file already exists.", org.Name)
		return nil, err
	}

	policyPath := makePolicyPath(org, policyRoot)
	adp := fileadapter.NewAdapter(policyPath)
	return adp, nil
}

func makePolicyPath(org *organization.Organization, policyRoot string) string {
	fn := fmt.Sprintf(policyFileFmt, org.Name)
	policyPath := path.Join(policyRoot, fn)
	return policyPath
}

// TODO: don't pass in policyRoot -- it won't be too relevant with the DB
// versions
func policyExists(org *organization.Organization, policyRoot string) bool {
	policyPath := makePolicyPath(org, policyRoot)
	_, err := os.Stat(policyPath)
	return !os.IsNotExist(err)
}

func initializePolicy(org *organization.Organization, policyRoot string) error {
	logger.Debugf("initializing policy!")
	if policyExists(org, policyRoot) {
		perr := fmt.Errorf("ACL policy for organization %s already exists, cannot initialize!", org.Name)
		return perr
	}

	policyPath := makePolicyPath(org, policyRoot)
	p, err := os.OpenFile(policyPath, os.O_RDWR|os.O_CREATE, 0600)
	if err != nil {
		return err
	}
	defer p.Close()
	if _, err = p.WriteString(defaultPolicySkel); err != nil {
		return err
	}
	return nil
}

func (c *Checker) waitForChanLock() {
	// later, someday, this may need to be org-specific, but not to-day
	// Block until the chan is free so we can hopefully work without getting
	// stepped on.
	ACLCoordinator <- struct{}{}
	return
}

func (c *Checker) releaseChanLock() {
	_ = <-ACLCoordinator
	return
}

func (c *Checker) testForAnyPol(item aclhelper.Item, doer aclhelper.Member, perm string) bool {
	// fi := c.e.GetFilteredPolicy(condNamePos, item.GetName())
	// Try getting this *user's* filtered policies, and make the test below
	// more specific.
	//
	// Sigh. We're looking at some special case code for groups. If we're
	// checking a group's perms, any existing perms on that group mean we
	// should not fall back to the general. Define 'fi', then load it with
	// either the user-oriented filtered policy, or the one filtered on
	// item name.
	//var fi [][]string
	//if item.ContainerKind() == "containers" {
	fi := c.e.GetFilteredPolicy(condGroupPos, doer.ACLName())
	//} else {
	//	fi = c.e.GetFilteredPolicy(condNamePos, item.GetName())
	//}

	if fi != nil && len(fi) != 0 {
		for _, p := range fi {
			logger.Debugf("testForAnyPol p for entity (%s) (%s): %v", doer.ACLName(), p[condGroupPos], p)
			logger.Debugf("what in God's name is getting missed here?\nitem.ContainerKind(): %s\tp[condKindPos]: %s\nitem.ContainerType(): %s\tp[condSubkindPos]: %s\nitem.GetName(): %s\tp[condNamePos]: %s\nperm: %s\tp[condPermPos]: %s", item.ContainerKind(), p[condKindPos], item.ContainerType(), p[condSubkindPos], item.GetName(), p[condNamePos], perm, p[condPermPos])
			// DON'T include perm!
			if item.ContainerKind() == p[condKindPos] && item.ContainerType() == p[condSubkindPos] && item.GetName() == p[condNamePos] {
				logger.Debugf("testForAnyPol returned true")
				return true
			}
		}
	}
	// Also check for a relevant denyall##groups. (sigh)
	if item.ContainerKind() == "groups" {
		logger.Debugf("testing 'denyall##groups' against %s", item.GetName())
		denyallp := buildDenySlice(item, perm)
		dnyChk := c.e.Enforce(denyallp...)
		logger.Debugf("denyall##groups check (p: (%v)) returned %v", denyallp, dnyChk)
		return dnyChk // d'oh, need to invert this
	}

	return false
}

func (c *Checker) CheckItemPerm(item aclhelper.Item, doer aclhelper.Actor, perm string) (bool, util.Gerror) {
	c.waitForChanLock()
	defer c.releaseChanLock()
	c.m.RLock()
	defer c.m.RUnlock()

	// grrr. Try reloading the policy every frickin' time we do anything.
	if polErr := c.e.LoadPolicy(); polErr != nil {
		return false, util.CastErr(polErr)
	}

	specific := buildEnforcingSlice(item, doer, perm)
	var chkSucceeded bool
	logger.Debugf("enforcing slice: %+v", specific)

	// try the specific check first, then the general
	if chkSucceeded = c.e.Enforce(specific...); !chkSucceeded {
		if !c.testForAnyPol(item, doer, perm) {
			logger.Debugf("trying the general: %+v", specific.general())
			chkSucceeded = c.e.Enforce(specific.general()...)
		} else {
			logger.Debugf("Not testing the general here - item-specific %s perms have been set for %s %s", perm, item.ContainerType(), item.GetName())
		}
	}
	if chkSucceeded {
		return true, nil
	}

	// check out failure conditions
	if !c.isPermValid(item, perm) {
		err := util.Errorf("invalid perm %s for %s-%s", perm, item.ContainerKind(), item.ContainerType())
		return false, err
	}

	err := testAssociation(doer, c.org)
	if err != nil {
		return false, err
	}

	return false, nil
}

// I won't pretend that I love this, but all we need to do here is test whether
// an association exists at all, not actually do anything with it. By not
// including the assocation library in this one, it will vastly simplify
// processing association requests, so that's something.
func testAssociation(doer aclhelper.Actor, org *organization.Organization) util.Gerror {
	if doer.IsUser() {
		// This will be much easier with a DB. Alas.
		if config.UsingDB() {

		} else {
			ds := datastore.New()
			key := util.JoinStr(doer.GetName(), "-", org.Name)
			if _, found := ds.Get("association", key); !found {
				err := util.Errorf("'%s' not associated with organization '%s'", doer.GetName(), org.Name)
				err.SetStatus(http.StatusForbidden)
				return err
			}
		}
	} else {
		if doer.OrgName() != org.Name {
			err := util.Errorf("client %s is not associated with org %s", doer.GetName(), org.Name)
			err.SetStatus(http.StatusForbidden)
			return err
		}
	}
	return nil
}

func (c *Checker) EditItemPerm(item aclhelper.Item, member aclhelper.Member, perms []string, action string) util.Gerror {
	c.waitForChanLock()
	defer c.releaseChanLock()
	c.m.Lock()
	defer c.m.Unlock()
	if polErr := c.e.LoadPolicy(); polErr != nil {
		return util.CastErr(polErr)
	}

	var policyFunc func(p ...interface{}) bool

	logger.Debugf("lol what: %s %s", item.GetName(), action)

	switch action {
	case addPerm:
		policyFunc = c.e.AddPolicy
	case removePerm:
		policyFunc = c.e.RemovePolicy
	default:
		return util.Errorf("invalid edit perm action '%s'", action)
	}

	if len(perms) == 0 {
		return util.Errorf("No permissions given to edit")
	}
	for _, p := range perms {
		if !checkValidPerm(p) {
			return util.Errorf("invalid perm '%s'", p)
		}
		pcondition := buildEnforcingSlice(item, member, p)
		policyFunc(pcondition...)
	}

	if err := c.e.SavePolicy(); err != nil {
		return util.CastErr(err)
	}

	return nil
}

func (c *Checker) EditFromJSON(item aclhelper.Item, perm string, data interface{}) util.Gerror {
	c.waitForChanLock()
	defer c.releaseChanLock()
	switch data := data.(type) {
	case map[string]interface{}:
		if _, ok := data[perm]; !ok {
			return util.Errorf("acl %s missing from JSON", perm)
		}
		c.m.Lock()
		defer c.m.Unlock()
		switch aclEdit := data[perm].(type) {
		case map[string]interface{}:
			// ----------
			// Implementation note: for each doer already in the
			// ACL, we'll need to check and see if they're present
			// in the new list. If not, they'll need to be removed.
			if polErr := c.e.LoadPolicy(); polErr != nil {
				return util.CastErr(polErr)
			}

			filteredItem := c.e.GetFilteredPolicy(condNamePos, item.GetName())
			logger.Debugf("FILTERED ITEM (in EditFromJSON) %s:\n####\n%s\n%%%%", item.GetName(), filteredItem)
			// logger.Debugf("FILTERED TYPE: %s\n####\n%s\n%%%%", item.ContainerType(), filteredType)
			newActRaw, ok := aclEdit["actors"].([]interface{})
			if !ok {
				return util.Errorf("invalid type for actor in acl")
			}
			newGroupsRaw, ok := aclEdit["groups"].([]interface{})
			if !ok {
				return util.Errorf("invalid type for group in acl")
			}
			newActors := make([]string, len(newActRaw))
			newGroups := make([]string, len(newGroupsRaw))
			for i, v := range newActRaw {
				newActors[i] = v.(string)
			}
			for i, v := range newGroupsRaw {
				newGroups[i] = v.(string)
			}

			for _, p := range filteredItem {
				logger.Debugf("checking p: %s", p)
				if p[condKindPos] == item.ContainerKind() && p[condSubkindPos] == item.ContainerType() && p[condPermPos] == perm {
					subj := p[condGroupPos]
					// skip any "denyall##groups" here, and
					// remove it further down if necessary
					if strings.HasPrefix(subj, "denyall##") {
						continue
					} else if strings.HasPrefix(subj, "role##") {
						if !util.StringPresentInSlice(strings.TrimPrefix(subj, "role##"), newGroups) {
							pi := make([]interface{}, len(p))
							for i, v := range p {
								pi[i] = v
							}
							c.e.RemovePolicy(pi...)
						}
					} else {
						if !util.StringPresentInSlice(subj, newActors) {
							pi := make([]interface{}, len(p))
							for i, v := range p {
								pi[i] = v
							}
							c.e.RemovePolicy(pi...)
						}
					}
				}
			}

			// may need later to permit allow/deny effect editing
			// Bizarrely both of thse are supposed to return 400
			// if the actor or group is not present
			// If, by chance, there are no groups provided for this
			// perm, add a special subject to the ACL:
			// "denyall##groups". Ugh.
			for _, act := range newActors {
				a, err := actor.GetActor(c.org, act)
				if err != nil {
					err.SetStatus(http.StatusBadRequest)
					return err
				}
				p := buildEnforcingSlice(item, a, perm)
				c.e.AddPolicy(p...)
			}

			// Here comes the science^W special case code! Alas,
			// using buildEnforcingSlice doesn't really work here,
			// so build it by hand.
			denyallp := buildDenySlice(item, perm)

			if len(newGroups) > 0 {
				for _, gr := range newGroups {
					g, err := group.Get(c.org, gr)
					if err != nil {
						err.SetStatus(http.StatusBadRequest)
						return err
					}
					p := buildEnforcingSlice(item, g, perm)
					c.e.AddPolicy(p...)
				}
				// remove "denyall##groups" if it's present and
				// this is a group. (It *appears* that this is
				// only proper for groups, but I could be
				// reading the tests wrong.)
				if item.ContainerKind() == "groups" {
					c.e.RemovePolicy(denyallp...)
				}
			} else if item.ContainerKind() == "groups" {
				logger.Debugf("adding denyall (%v) to item %s", denyallp, item.GetName())
				// No groups, so we add the denyall
				c.e.AddPolicy(denyallp...)
			}
		default:
			return util.Errorf("invalid acl %s data", perm)
		}
	default:
		return util.Errorf("invalid acl data")
	}
	if err := c.e.SavePolicy(); err != nil {
		return util.CastErr(err)
	}
	return nil
}

func (c *Checker) RootCheckPerm(doer aclhelper.Actor, perm string) (bool, util.Gerror) {
	return c.CheckItemPerm(c.org, doer, perm)
}

func (c *Checker) CheckContainerPerm(doer aclhelper.Actor, containerName string, perm string) (bool, util.Gerror) {
	cont, err := container.Get(c.org, containerName)
	if err != nil {
		return false, err
	}
	return c.CheckItemPerm(cont, doer, perm)
}

func buildEnforcingSlice(item aclhelper.Item, member aclhelper.Member, perm string) enforceCondition {
	cond := []interface{}{member.ACLName(), item.ContainerType(), item.ContainerKind(), item.GetName(), perm, enforceEffect}
	return enforceCondition(cond)
}

func buildDenySlice(item aclhelper.Item, perm string) enforceCondition {
	denyCond := []interface{}{"denyall##groups", item.ContainerType(), item.ContainerKind(), item.GetName(), perm, enforceEffect}
	return enforceCondition(denyCond)
}

func (e enforceCondition) general() enforceCondition {
	g := make([]interface{}, len(e))
	// Trying something here: if the Type and Kind are both "container",
	// then Type (subkind) should be switched to the GetName() value,
	// because containers are kind of weird.
	for i, v := range e {
		g[i] = v
	}
	if g[condSubkindPos] == "containers" && g[condKindPos] == "containers" {
		g[condSubkindPos] = g[condNamePos]
	}
	g[condNamePos] = "$$default$$"
	return enforceCondition(g)
}

func (c *Checker) isPermValid(item aclhelper.Item, perm string) bool {
	// pare down the list to check a little
	fPass := c.e.GetFilteredPolicy(condSubkindPos, item.ContainerType())
	validPerms := make(map[string]bool)
	for _, p := range fPass {
		if p[condKindPos] == item.ContainerKind() {
			validPerms[p[condPermPos]] = true
		}
	}
	return validPerms[perm]
}

// TODO: Determine what's actually needed with these...? There might not be much
// for this.
func (c *Checker) AddACLRole(gRole aclhelper.Role) error {
	c.waitForChanLock()
	defer c.releaseChanLock()
	// If there's any members in the role, add them. Otherwise, there's
	// not anything to do.
	logger.Debugf("Running AddACLRole, calling AddMembers on all members in group %s", gRole.GetName())
	c.m.Lock()
	defer c.m.Unlock()
	c.inTransaction = true
	defer func() {
		c.inTransaction = false
	}()

	if polErr := c.e.LoadPolicy(); polErr != nil {
		return util.CastErr(polErr)
	}
	return c.AddMembers(gRole, gRole.AllMembers())
}

func (c *Checker) RemoveACLRole(gRole aclhelper.Role) error {
	c.waitForChanLock()
	defer c.releaseChanLock()
	c.m.Lock()
	defer c.m.Unlock()
	c.inTransaction = true
	defer func() {
		c.inTransaction = false
	}()

	if polErr := c.e.LoadPolicy(); polErr != nil {
		return util.CastErr(polErr)
	}
	c.e.DeleteRole(gRole.ACLName())
	return c.e.SavePolicy()
}

func (c *Checker) AddMembers(gRole aclhelper.Role, adding []aclhelper.Member) error {
	if !c.inTransaction {
		c.waitForChanLock()
		defer c.releaseChanLock()
		c.m.Lock()
		defer c.m.Unlock()
	}

	if polErr := c.e.LoadPolicy(); polErr != nil {
		return util.CastErr(polErr)
	}
	for _, m := range adding {
		c.e.AddRoleForUser(m.ACLName(), gRole.ACLName())
	}
	logger.Debugf("added %d members to %s ACL role", len(adding), gRole.GetName())

	return c.e.SavePolicy()
}

func (c *Checker) RemoveMembers(gRole aclhelper.Role, removing []aclhelper.Member) error {
	if !c.inTransaction {
		c.waitForChanLock()
		defer c.releaseChanLock()
		c.m.Lock()
		defer c.m.Unlock()
	}

	if polErr := c.e.LoadPolicy(); polErr != nil {
		return util.CastErr(polErr)
	}
	for _, m := range removing {
		c.e.DeleteRoleForUser(m.ACLName(), gRole.ACLName())
	}
	logger.Debugf("deleted %d members from %s ACL role", len(removing), gRole.GetName())

	return c.e.SavePolicy()
}

func (c *Checker) RemoveUser(u aclhelper.Member) error {
	c.m.Lock()
	defer c.m.Unlock()

	if polErr := c.e.LoadPolicy(); polErr != nil {
		return util.CastErr(polErr)
	}
	c.e.DeleteRolesForUser(u.ACLName())
	logger.Debugf("deleted all ACL perms for %s", u.ACLName())
	return c.e.SavePolicy()
}

func (c *Checker) RemoveItemACL(item aclhelper.Item) util.Gerror {
	return nil
}

func (c *Checker) Enforcer() *casbin.SyncedEnforcer {
	return c.e
}

func (c *Checker) GetItemACL(item aclhelper.Item) (*aclhelper.ACL, error) {
	c.waitForChanLock()
	defer c.releaseChanLock()
	c.m.RLock()
	defer c.m.RUnlock()

	if polErr := c.e.LoadPolicy(); polErr != nil {
		return nil, util.CastErr(polErr)
	}
	// Hrmph, it'd be nice if this was a little easier. At least here we
	// can get it by name and do the kind/subkind checks afterwards.
	filteredItem := c.e.GetFilteredPolicy(condNamePos, item.GetName())

	// Buh. The filtered type is different if it's a group we're dealing
	// with.
	var filteredType [][]string

	if item.ContainerKind() == "groups" {
		filteredType = c.e.GetFilteredPolicy(condNamePos, "$$default$$")
	} else {
		filteredType = c.e.GetFilteredPolicy(condSubkindPos, item.ContainerType())
	}

	if (filteredItem == nil || len(filteredItem) == 0) && (filteredType == nil || len(filteredType) == 0) {
		err := fmt.Errorf("item '%s' (and overall type '%s') not found in ACL", item.GetName(), item.ContainerType())
		return nil, err
	}

	// COME ON!
	logger.Debugf("FILTERED ITEM %s:\n####\n%s\n%%%%", item.GetName(), filteredItem)
	logger.Debugf("FILTERED TYPE: %s\n####\n%s\n%%%%", item.ContainerType(), filteredType)

	itemCompare := func(i aclhelper.Item, pol []string) bool {
		return pol[condKindPos] == i.ContainerKind() && pol[condSubkindPos] == i.ContainerType()
	}
	genCompare := func(i aclhelper.Item, pol []string) bool {
		// short circuit the check below if we're in the weird case
		// where we're assembling perms for a new container. How often
		// does that actually come up, anyway?
		if i.ContainerKind() == "containers" && i.ContainerType() == "containers" {
			return false
		}

		// weird as it seems, this may be OK.
		return pol[condKindPos] == i.ContainerKind()
	}

	itemPerms := assembleACL(item, filteredItem, itemCompare)
	genPerms := assembleACL(item, filteredType, genCompare)
	// wtf is in here
	for k, v := range genPerms.Perms {
		logger.Debugf("arrgh: %s :: %+v", k, v)
	}

	// Sigh, a special corner case with custom containers.
	if item.ContainerKind() == "containers" && item.ContainerType() == "containers" {
		// just set genPerms to itemPerms in this weird-ish situation
		genPerms = itemPerms
	} else { // the normal case
		// Override general permissions with the specifics
		for k, v := range itemPerms.Perms {
			genPerms.Perms[k].Perm = v.Perm
			genPerms.Perms[k].Effect = v.Effect
			if v.Actors != nil {
				genPerms.Perms[k].Actors = v.Actors
			}
			if v.Groups != nil {
				genPerms.Perms[k].Groups = v.Groups
			}
		}
	}
	for _, v := range genPerms.Perms {
		if !util.StringPresentInSlice(DefaultUser, v.Actors) {
			v.Actors = append(v.Actors, DefaultUser)
		}
		// also, remove any dupes in Actors or Groups
		v.Actors = util.RemoveDupStrings(v.Actors)
		v.Groups = util.RemoveDupStrings(v.Groups)
	}
	for k, v := range genPerms.Perms {
		logger.Debugf("GetItemACL %s Actors: %v", k, v.Actors)
		logger.Debugf("GetItemACL %s Groups: %v", k, v.Groups)
	}

	return genPerms, nil
}

func (c *Checker) GetItemPolicies(itemName string, itemKind string, itemType string) [][]interface{} {
	c.e.LoadPolicy() // maybe handle errs later
	filteredItem := c.e.GetFilteredPolicy(condNamePos, itemName)
	if filteredItem == nil || len(filteredItem) == 0 {
		return nil
	}
	policies := make([][]interface{}, 0)
	for _, p := range filteredItem {
		if p[condKindPos] == itemKind && p[condSubkindPos] == itemType {
			pface := make([]interface{}, len(p))
			for i, v := range p {
				pface[i] = v
			}
			policies = append(policies, pface)
		}
	}
	return policies
}

func (c *Checker) RenameItemACL(item aclhelper.Item, oldName string) error {
	c.waitForChanLock()
	defer c.releaseChanLock()
	c.m.Lock()
	defer c.m.Unlock()

	if polErr := c.e.LoadPolicy(); polErr != nil {
		return util.CastErr(polErr)
	}
	oldPolicies := c.GetItemPolicies(oldName, item.ContainerKind(), item.ContainerType())
	if oldPolicies == nil || len(oldPolicies) == 0 {
		return nil
	}
	for _, p := range oldPolicies {
		newPolicy := make([]interface{}, len(p))
		copy(newPolicy, p)
		newPolicy[condNamePos] = item.GetName()
		c.e.AddPolicy(newPolicy...)
	}
	// Wait until all new policies have been added before deleting the old
	// ones.
	for _, p := range oldPolicies {
		if _, err := c.e.RemovePolicySafe(p...); err != nil {
			return err
		}
	}
	return c.e.SavePolicy()
}

func (c *Checker) RenameMember(member aclhelper.Member, oldName string) error {
	c.waitForChanLock()
	defer c.releaseChanLock()
	c.m.Lock()
	defer c.m.Unlock()

	if polErr := c.e.LoadPolicy(); polErr != nil {
		return util.CastErr(polErr)
	}
	oldPol := c.e.GetPermissionsForUser(oldName)
	if oldPol == nil || len(oldPol) == 0 {
		return nil
	}
	oldPolicies := make([][]interface{}, len(oldPol))
	for i, p := range oldPol {
		np := make([]interface{}, len(p))
		for z, v := range p {
			np[z] = v
		}
		oldPolicies[i] = np
	}

	for _, p := range oldPolicies {
		newPolicy := make([]interface{}, len(p))
		copy(newPolicy, p)
		newPolicy[condGroupPos] = member.ACLName()
		c.e.AddPolicy(newPolicy...)
	}
	for _, p := range oldPolicies {
		if _, err := c.e.RemovePolicySafe(p...); err != nil {
			return err
		}
	}
	return c.e.SavePolicy()
}

func (c *Checker) DeleteItemACL(item aclhelper.Item) (bool, error) {
	c.waitForChanLock()
	defer c.releaseChanLock()
	c.m.Lock()
	defer c.m.Unlock()

	logger.Debugf("DeleteItemACL #1")
	if polErr := c.e.LoadPolicy(); polErr != nil {
		return false, util.CastErr(polErr)
	}
	logger.Debugf("DeleteItemACL #2")
	policies := c.GetItemPolicies(item.GetName(), item.ContainerKind(), item.ContainerType())

	logger.Debugf("DeleteItemACL #3")

	var rmok bool
	var err error

	for _, p := range policies {
		if rmok, err = c.e.RemovePolicySafe(p...); err != nil {
			return false, err
		}
	}

	logger.Debugf("DeleteItemACL #4")
	if err := c.e.SavePolicy(); err != nil {
		return false, err
	}
	logger.Debugf("DeleteItemACL #5")
	return rmok, nil
}

func (c *Checker) CreatorOnly(item aclhelper.Item, creator aclhelper.Actor) util.Gerror {
	if polErr := c.e.LoadPolicy(); polErr != nil {
		return util.CastErr(polErr)
	}
	// hmm?
	for _, p := range aclhelper.DefaultACLs {
		err := c.EditItemPerm(item, creator, []string{p}, "add")
		if err != nil {
			return err
		}
	}
	return nil
}

func assembleACL(item aclhelper.Item, filtered [][]string, comparer func(aclhelper.Item, []string) bool) *aclhelper.ACL {
	tmpACL := new(aclhelper.ACL)
	tmpACL.Perms = make(map[string]*aclhelper.ACLItem)

	for _, p := range filtered {
		if comparer(item, p) {
			perm := p[condPermPos]
			subj := p[condGroupPos]
			eft := p[condEffectPos]

			// skip over the perm item if its effect is "deny".
			// I'm not ruling out somewhere down the line breaking
			// strict Chef Server compat with ACLs, though, and
			// making it fit better with how casbin does it. We'll
			// see, though. Regardless, do this for now to avoid
			// unexpected items poping up in the acl JSON.
			if eft == denyEffect {
				continue
			}

			if _, ok := tmpACL.Perms[perm]; !ok {
				tmpACL.Perms[perm] = new(aclhelper.ACLItem)
				//tmpACL.Perms[perm].Actors = make([]string, 0)
				//tmpACL.Perms[perm].Groups = make([]string, 0)
				tmpACL.Perms[perm].Actors = nil
				tmpACL.Perms[perm].Groups = nil
				tmpACL.Perms[perm].Perm = perm
				tmpACL.Perms[perm].Effect = p[condEffectPos]
			}
			if strings.HasPrefix(subj, "role##") {
				gname := strings.TrimPrefix(subj, "role##")
				tmpACL.Perms[perm].Groups = append(tmpACL.Perms[perm].Groups, gname)
			} else {
				//if !isValidator(item) {
				// Hmm. Again.
				logger.Debugf("assembling acl: are we a validator? %v", isValidator(item))
				tmpACL.Perms[perm].Actors = append(tmpACL.Perms[perm].Actors, subj)
				//}
			}
		}
	}

	return tmpACL
}

func isValidator(item aclhelper.Item) bool {
	if cl, ok := item.(aclhelper.Actor); ok {
		if cl.IsClient() {
			return cl.IsValidator()
		}
	}

	return false
}

func checkValidPerm(perm string) bool {
	for _, p := range aclhelper.DefaultACLs {
		if p == perm {
			return true
		}
	}
	return false
}
