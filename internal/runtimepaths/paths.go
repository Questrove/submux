package runtimepaths

type Defaults struct {
	StateRoot string
	Endpoint  string
	LockFile  string
}

func Current() Defaults {
	return platformDefaults()
}
