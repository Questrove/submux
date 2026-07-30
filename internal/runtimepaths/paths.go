package runtimepaths

type Defaults struct {
	StateRoot       string
	Endpoint        string
	LockFile        string
	ControlEndpoint string
	NetworkEndpoint string
}

func Current() Defaults {
	return platformDefaults()
}
