/*******************************************************************************
*
* Copyright 2018 SAP SE
*
* Licensed under the Apache License, Version 2.0 (the "License");
* you may not use this file except in compliance with the License.
* You should have received a copy of the License along with this
* program. If not, you may obtain a copy of the License at
*
*     http://www.apache.org/licenses/LICENSE-2.0
*
* Unless required by applicable law or agreed to in writing, software
* distributed under the License is distributed on an "AS IS" BASIS,
* WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
* See the License for the specific language governing permissions and
* limitations under the License.
*
*******************************************************************************/

package keppel

import (
	"errors"
	"net/http"
)

//Permission is an enum used by AuthDriver.
type Permission string

const (
	//CanViewAccount is the permission for viewing account metadata.
	CanViewAccount Permission = "view"
	//CanPullFromAccount is the permission for pulling images from this account.
	CanPullFromAccount = "pull"
	//CanPushToAccount is the permission for pushing images to this account.
	CanPushToAccount = "push"
	//CanChangeAccount is the permission for creating and updating accounts.
	CanChangeAccount = "change"
)

//Authorization describes the access rights for a user. It is returned by
//methods in the AuthDriver interface.
type Authorization interface {
	HasPermission(perm Permission, tenantID string) bool
}

//AuthDriver represents an authentication backend that supports multiple
//tenants. A tenant is a scope where users can be authorized to perform certain
//actions. For example, in OpenStack, a Keppel tenant is a Keystone project.
type AuthDriver interface {
	//ReadConfig unmarshals the configuration for this driver type into this
	//driver instance. The `unmarshal` function works exactly like in
	//UnmarshalYAML. This method shall only fail if the input data is malformed.
	//It shall not make any network requests; use Connect for that.
	ReadConfig(unmarshal func(interface{}) error) error
	//Connect prepares this driver instance for usage. This is called *after*
	//ReadConfig and *before* any other methods are called.
	Connect() error

	//ValidateTenantID checks if the given string is a valid tenant ID. If so,
	//nil shall be returned. If not, the returned error shall explain why the ID
	//is not valid. The driver implementor can decide how thorough this check
	//shall be: It can be anything from "is not empty" to "matches regex" to
	//"exists in the auth database".
	ValidateTenantID(tenantID string) error

	//SetupAccount sets up the given tenant so that it can be used for the given
	//Keppel account. The caller must supply an Authorization that was obtained
	//from one of the AuthenticateUserXXX methods of the same instance, because
	//this operation may require more permissions than Keppel itself has.
	SetupAccount(account Account, an Authorization) error

	//AuthenticateUser authenticates the user identified by the given username
	//and password. Note that usernames may not contain colons, because
	//credentials are encoded by clients in the "username:password" format.
	AuthenticateUser(userName, password string) (Authorization, *RegistryV2Error)
	//AuthenticateUserFromRequest reads credentials from the given incoming HTTP
	//request to authenticate the user which makes this request. The
	//implementation shall follow the conventions of the concrete backend, e.g. a
	//OAuth backend could try to read a Bearer token from the Authorization
	//header, whereas an OpenStack auth driver would look for a Keystone token in the
	//X-Auth-Token header.
	AuthenticateUserFromRequest(r *http.Request) (Authorization, *RegistryV2Error)
}

var authDriverFactories = make(map[string]func() AuthDriver)

//NewAuthDriver creates a new AuthDriver using one of the factory functions
//registered with RegisterAuthDriver().
func NewAuthDriver(name string) (AuthDriver, error) {
	factory := authDriverFactories[name]
	if factory != nil {
		return factory(), nil
	}
	return nil, errors.New("no such auth driver: " + name)
}

//RegisterAuthDriver registers an AuthDriver. Call this from func init() of the
//package defining the AuthDriver.
func RegisterAuthDriver(name string, factory func() AuthDriver) {
	if _, exists := authDriverFactories[name]; exists {
		panic("attempted to register multiple auth drivers with name = " + name)
	}
	authDriverFactories[name] = factory
}
