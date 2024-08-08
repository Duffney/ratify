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
