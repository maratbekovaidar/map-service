# Custom OSRM profiles

The stack uses the **stock** profiles shipped in the OSRM image (`/opt/car.lua`,
`/opt/foot.lua`, `/opt/bicycle.lua`). Put a file named `<profile>.lua` here only
if you really need to change routing behaviour (for example `foot.lua` that avoids
stairs); the updater then mounts this directory at `/profiles` and runs
`osrm-extract -p /profiles/<profile>.lua` for that profile instead of the stock
one. Profile names are those of `ROUTING_PROFILES`.

Start from the matching file of the pinned OSRM release:

```bash
docker run --rm --entrypoint cat ghcr.io/project-osrm/osrm-backend:v26.10.0-debian /opt/foot.lua > deploy/osrm/foot.lua
```

Profiles `require("lib/...")` helper modules; those resolve to the copies inside the
image, so only the `.lua` file itself has to be placed here (verified with
`osrm-extract -p /profiles/foot.lua` on a copy of the city extract).
