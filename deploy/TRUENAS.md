# Deploying MultiClaude on TrueNAS SCALE, reachable from the internet

What you end up with:

- `https://multiclaude.example.com:8943` — the UI and git, with a Let's Encrypt certificate, from anywhere
- `http://<nas-ip>:8090` — the same thing over plain HTTP, inside your LAN only
- Login with username + password for people, a per-machine token for runners

It doesn't need ports 80/443. Those stay free for the TrueNAS UI or a reverse proxy for your other apps.

Tested with TrueNAS SCALE 24.10 (Electric Eel) and later. These versions run apps with Docker.

## 1. Domain and DNS API token

The certificate comes from Let's Encrypt via the **DNS challenge**: MultiClaude briefly creates a TXT record through your DNS provider's API. That works on any port. Supported providers: **Cloudflare** and **IONOS**.

**The name**: an `A` record for e.g. `multiclaude.example.com` pointing to your public IP. On Cloudflare it must be *DNS only* (grey cloud), because the proxy doesn't pass port 8943. If your IP changes, keep the record current with your router's DynDNS. On IONOS, `MC_DYNDNS: "on"` can do it for you.

**Cloudflare token**:

1. Go to *My Profile → API Tokens → Create Token → "Edit zone DNS" template*.
2. Permissions: *Zone · DNS · Edit*. Add *Zone · Zone · Read*.
3. Zone Resources: *Include · Specific zone · example.com*.
4. Put the token in `MC_DNS_API_KEY` with `MC_DNS_PROVIDER: cloudflare`.

**IONOS key**: at <https://developer.hosting.ionos.de> → *API Keys*. Use `prefix.secret` with `MC_DNS_PROVIDER: ionos`.

Check from outside: `dig +short multiclaude.example.com @1.1.1.1` should print your public IP.

> If your provider uses CGNAT (no public IPv4; common on cable and mobile), port forwarding can't work. Use a Cloudflare Tunnel or Tailscale Funnel to port 8090 instead, and leave `MC_DOMAIN` empty.

## 2. Port forwarding on the router

Forward **external TCP 8943 → NAS IP, port 8943**. A range forward such as 8500–9000 covers it.

On a FritzBox: *Internet → Permit Access → Port Sharing → the NAS → New sharing → Port sharing*, TCP 8943 to 8943.

**Never forward 8090**. It is outside such a range on purpose. Plain HTTP must stay inside the LAN.

## 3. Storage

*Datasets → Add Dataset*, e.g. `tank/apps/multiclaude`. Then *Edit Permissions*: owner user **apps**, group **apps** (uid/gid 568).

This dataset holds everything:

- git repositories and LFS files
- the SQLite database
- certificates

Include it in your snapshot/replication tasks.

## 4. The image

The NAS can't build the image itself. Build it on your workstation and pull it from the NAS.

**Without SSH** (easiest). On your workstation, in the repo:

```sh
make serve-image
```

This builds the image, serves it on port 8000 and prints a command like:

```sh
curl -fL http://192.168.1.20:8000/multiclaude.tar -o /tmp/multiclaude.tar && sudo docker load -i /tmp/multiclaude.tar && rm /tmp/multiclaude.tar
```

Paste that into TrueNAS → *System → Shell*. When it says `Loaded image: …multiclaude:latest`, stop `make serve-image` with Ctrl+C. Only the tar file is served, nothing else from your computer.

**With SSH**: enable *System → Services → SSH*. On TrueNAS 24.10+ root can't log in over SSH; use the admin user (`truenas_admin`), and add your public key under *Credentials → Users → Edit → Authorized Keys*. Then:

```sh
make deploy-nas NAS=truenas_admin@<nas-ip>
```

Use the IP if `truenas.local` doesn't resolve. `sudo` must work without a password for `docker`: in the user's settings, add `/usr/bin/docker` to *Allowed sudo commands with no password*.

**Updates**: load a new image the same way, then *Apps → multiclaude → Restart*. The database migrates itself on start.

## 5. Install the app

*Apps → Discover Apps → ⋮ (top right) → Install via YAML*. Name it `multiclaude` and paste [`docker-compose.yml`](docker-compose.yml), after editing these values:

- `MC_DOMAIN` — your subdomain from step 1
- `MC_DNS_PROVIDER` / `MC_DNS_API_KEY` — `cloudflare` + API token (or `ionos` + `prefix.secret`)
- the volume host path — your dataset from step 3
- optionally `MC_ADMIN_PASSWORD`; otherwise a random password is generated

Save. Then open *Apps → multiclaude → Logs*. The first start prints:

```
==> First start: created user "admin" with password  xxxxxxxxxxxxxxxxxxxx
```

The password is also saved in `initial-admin-password.txt` in the dataset. Log in, then change it on **Users & account**. That deletes the file.

The logs then show:

```
tls: requesting a Let's Encrypt certificate for multiclaude.example.com (DNS challenge)…
tls: certificate installed, valid until …
```

That takes about 1–3 minutes, waiting for DNS to propagate. Until then HTTPS uses a temporary self-signed certificate, so browsers warn. Renewal is automatic, with a third of the lifetime left. The certificate is stored in the dataset under `certs/`.

To test without risking Let's Encrypt rate limits, set `MC_ACME_DIRECTORY: https://acme-staging-v02.api.letsencrypt.org/directory` first. Browsers don't trust staging certificates. Then remove the setting, delete `certs/` and restart.

## 6. Connect the machines

Open the UI under the address the machines will use: `https://multiclaude.example.com:8943` for machines anywhere, or `http://<nas-ip>:8090` for LAN-only machines. Go to **Machines → Add a machine** and copy the command:

```sh
curl -fsSL https://multiclaude.example.com:8943/install.sh | sh -s -- --token mcj_…
```

Run it on each computer as the user that is logged into Claude Code. What it does:

1. Enrolls the machine with the **join token**.
2. Stores the machine's **own token** in `~/.multiclaude/credentials.json`.
3. Installs a user service.

The join token itself is not stored on the machine.

- **Regenerate join token** (Machines page): old install commands stop working. Enrolled machines keep running.
- **Remove** a machine: revokes its token immediately.
- **Re-enroll** a machine: run the install command again.

LAN machines can keep using `http://<nas-ip>:8090`. If a LAN machine should use the public name, your router must support NAT loopback (hairpin NAT). Otherwise add a local DNS entry that maps the name to the NAS IP. Most routers and Pi-hole can do that.

Already-running machines from before auth existed must be enrolled once. Re-run the installer with `--token`; they keep their identity and project assignments.

## 7. People and git

- **Users & account**: add users (everyone has full access), change passwords.
- Git uses the same accounts:
  ```sh
  git clone https://multiclaude.example.com:8943/git/<project>.git   # asks for username + password
  git config --global credential.helper store                   # or your OS keychain helper
  ```
- Scripts can call the API with Basic auth: `curl -u admin:… https://…/api/overview`.
- Forgot the password? In *System → Shell*:
  ```sh
  sudo docker exec multiclaude multiclaude-server reset-password admin
  ```

## Security notes

- **Brute-force protection**: after 5 failed logins from one IP, that IP is locked out, starting at 1 minute and doubling up to 16 minutes. Failed logins are logged in the app logs.
- **Sessions**: cookies are HttpOnly, SameSite=Lax and Secure over HTTPS, and expire after 30 days. Changing a password logs out that user's other sessions.
- **Machine tokens and agents**: agents on a machine can use that machine's token, because they run there with full permissions. Only enroll machines you trust.
- **The DNS API token** can change DNS records (Cloudflare: only in the zone you selected; IONOS: the whole account). It's stored only in the app configuration.
