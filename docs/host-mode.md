# Host-mode background processes on Windows

Windows host jobs clean up background step processes at job completion. A step can opt out:

```yaml
steps:
  - name: Start shared helper
    env:
      RUNNER_TRACKING_ID: ""
    run: Start-Process vendor-helper.exe
```

- Set the `RUNNER_TRACKING_ID` override in the step's `env` before it starts. Any value different from the job's ID opts out, including an empty string.
- Opted-out processes and descendants that inherit the override survive normal job cleanup, including workspace process sweeps and expired output waits. Detach helper output because the runner closes leftover output pipes after a bounded wait.
- Cancelling or timing out an active step still terminates its process tree.
- The override does not preserve workspace or temporary files. Store persistent helper files outside those directories and arrange the helper's eventual shutdown.

This follows [actions/runner's tracking ID and orphan cleanup](https://github.com/actions/runner/blob/main/src/Runner.Worker/JobExtension.cs). Container jobs follow their container's lifecycle.
