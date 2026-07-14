# buildbust examples

Two runnable scripts, both offline and self-contained.

## make-demo-context.sh

Fabricates a deterministic two-stage build context (Node-style app with a
`.dockerignore`) — the shape most cache mysteries come in. Use it to try
every buildbust subcommand without touching a real project:

```bash
bash examples/make-demo-context.sh /tmp/buildbust-demo
buildbust snapshot /tmp/buildbust-demo
echo '// edit' >> /tmp/buildbust-demo/src/server.js
buildbust explain /tmp/buildbust-demo
buildbust files /tmp/buildbust-demo
```

## pre-build-report.sh

Wraps a `docker build` with a culprit report: it explains what busted the
cache since the last build, re-baselines with `--update`, and then hands
off to the real build command. The docker invocation itself is commented
out so the example runs offline; uncomment it in your own wrapper.

```bash
bash examples/pre-build-report.sh /tmp/buildbust-demo -t demo:latest
```

Both scripts write fixed file contents and never call the network, so
their output is identical on every machine.
