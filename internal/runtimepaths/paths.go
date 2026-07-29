package runtimepaths

type Defaults struct {
	StateRoot       string
	Endpoint        string
	LockFile        string
	ControlEndpoint string
}

func Current() Defaults {
	return platformDefaults()
}
