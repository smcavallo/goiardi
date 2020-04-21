/*
 * Copyright (c) 2013-2020, Jeremy Bingham (<jeremy@goiardi.gl>)
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

/*
 * At last, goiardi's going to get policyfile support.
 */

package policies

import (
	"github.com/ctdk/goiardi/organization"
	"github.com/ctdk/goiardi/util"
)

type PolicyGroup struct {
	Name string
	Policies map[string]*Policy
}

func New() (*PolicyGroup, util.Gerror) {
	return nil, nil
}

func Get() (*PolicyGroup, util.Gerror) {
	return nil, nil
}
