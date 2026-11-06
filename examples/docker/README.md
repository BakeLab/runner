### Run `gitea-runner` in a Docker Container

The recommended Docker-in-Docker image runs its own Docker daemon for jobs, so they never get the host's Docker socket:

```sh
docker run --privileged \
  -e GITEA_INSTANCE_URL=http://192.168.8.18:3000 \
  -e GITEA_RUNNER_REGISTRATION_TOKEN=<runner_token> \
  -v "$PWD/data:/data" \
  -v runner-docker:/var/lib/docker \
  --name my_runner gitea/runner:latest-dind
```

The volumes keep the runner's registration and the job image cache. See [image flavours](../../README.md#image-flavours) for alternatives.

#### Pull job images from a registry with a private CA

The bundled Docker daemon pulls job images, so it needs to trust the registry's CA itself. `runner.ca_file` doesn't cover this. Mount the CA where the daemon looks for it, naming the directory after the registry's host and port:

```sh
--mount "type=bind,src=$PWD/ca.crt,dst=/etc/docker/certs.d/registry.example.com:5000/ca.crt,readonly"
```

Use `--mount`, since `-v` rejects a container path with a port in it.

On `latest-dind-rootless`, use `/home/rootless/.config/docker/certs.d/registry.example.com:5000/ca.crt` as the target.
