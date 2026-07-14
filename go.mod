// buildbust — explains exactly which file or Dockerfile line busted your
// Docker build cache, by hashing the build context per instruction.
//
// version:    0.1.0
// author:     JaydenCJ
// license:    MIT
// repository: https://github.com/JaydenCJ/buildbust
// keywords:   docker, buildkit, dockerfile, cache, build-context, ci, devops
//
// Zero runtime dependencies: standard library only.
module github.com/JaydenCJ/buildbust

go 1.22
