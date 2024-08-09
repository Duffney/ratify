/*
Copyright The Ratify Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

	http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/
package refresh

import "fmt"

var refresherFactories = make(map[string]RefresherFactory)

type RefresherFactory interface {
	Create(config map[string]interface{}) (Refresher, error)
}

func Register(name string, factory RefresherFactory) {
	if factory == nil {
		panic("refresher factory cannot be nil")
	}
	_, registered := refresherFactories[name]
	if registered {
		panic(fmt.Sprintf("refresher factory named %s already registered", name))
	}
	refresherFactories[name] = factory
}

func CreateRefresherFromConfig(refresherConfig map[string]interface{}) (Refresher, error) {
	refresherType, ok := refresherConfig["type"].(string)
	if !ok || refresherType == "" {
		return nil, fmt.Errorf("refresher type cannot be empty")
	}
	factory, ok := refresherFactories[refresherType]
	if !ok {
		return nil, fmt.Errorf("refresher factory with name %s not found", refresherType)
	}
	return factory.Create(refresherConfig)
}
