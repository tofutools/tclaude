package nodeinfo

import "context"

func osVersion(ctx context.Context) string { return output(ctx, "/usr/bin/sw_vers", "-productVersion") }
