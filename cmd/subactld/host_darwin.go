package main

import "github.com/zydo/subactl/internal/capacity"

func darwinHost() capacity.Reader { return capacity.DarwinReader{} }
