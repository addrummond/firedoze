 We’d need to add a build-from-recipe path that creates a VM, runs setup commands inside it, sleeps/stops it cleanly, then snapshots it as a named template.

  Rough model:

  FROM ubuntu-base

  RUN apt-get update
  RUN apt-get install -y nodejs npm
  COPY ./app /home/ubuntu/app
  RUN chown -R ubuntu:ubuntu /home/ubuntu/app

  EXPOSE 8080
  CMD systemctl start firedoze-hello

  But I would not literally implement Dockerfile semantics. I’d define a simpler Firedozefile that maps cleanly to VMs.

  What We Need

  1. A recipe format.

  Example:

  name = "node-dev"
  base = "ubuntu"

  [vm]
  memory_mib = 2048
  vcpus = 2
  disk_size = "20G"

  [[copy]]
  src = "./"
  dst = "/home/ubuntu/app"

  [[run]]
  cmd = "apt-get update && apt-get install -y nodejs npm"

  [[run]]
  user = "ubuntu"
  cmd = "cd /home/ubuntu/app && npm install"

  [default_route]
  port = 3000

  2. A builder command.

  Maybe:

  firedoze build -name node-dev ./Firedozefile

  or:

  firedoze image build node-dev

  Flow:

  create temporary VM
  start VM
  wait for SSH
  copy files in
  run recipe commands
  sleep or stop VM
  snapshot as named image/template
  delete temporary VM

  3. A distinction between snapshots and templates/images.

  Right now named snapshots are cloneable state. For a build pipeline, we probably want a user-facing concept like:

  firedoze image list
  firedoze image build node-dev
  firedoze vm create myapp -image node-dev

  Internally this can initially be a snapshot, but the UX should probably call it an image/template.

  4. Non-interactive VM exec/copy must be solid.

  We already have firedoze exec, and with-vm-ip can support rsync, but the builder needs first-class helpers:

  - run command and stream output
  - copy local files into VM
  - maybe copy files out
  - fail build on non-zero exit
  - enforce timeout
  - clean up temp VM on failure unless -keep-failed

  5. Clean finalization.

  Before snapshotting, builder should run something like:

  sync
  cloud-init clean || true
  journalctl --rotate || true
  journalctl --vacuum-time=1s || true
  rm -rf /tmp/*

  Then:

  firedoze vm sleep build-temp
  firedoze snapshot save node-dev build-temp

  Since we just disallowed running snapshots, this flow now fits well.

  6. Route/default metadata.

  The recipe should be able to set VM defaults used by new clones:

  - default HTTP port
  - public/hidden default
  - auto-wake default
  - maybe default command docs

  7. Rebuild behavior.

  Decide what happens when node-dev already exists:

  firedoze build -replace node-dev

  Without -replace, fail.

  8. Cache/layering?

  For v1, skip it. Every build starts from base image or named image and runs commands. ZFS clones make this less painful anyway.

  Implementation Shape

  Minimal v1:

  firedoze build <name> <recipe>

  Recipe supports:

  - memory_mib
  - vcpus
  - disk_bytes
  - run
  - copy
  - http_port

  Output is a named snapshot/template.

  Then:

  firedoze vm create myvm -from node-dev

  or:

  firedoze image restore node-dev myvm

  Main Design Decision

  I’d avoid copying Dockerfile too closely. Dockerfiles are layer-oriented and container-specific. Firedoze builds whole persistent computers. A small TOML or
  YAML recipe would be clearer and easier to implement correctly.
