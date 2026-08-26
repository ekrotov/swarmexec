# LinkedIn release announcements — internal setup

Internal ops doc for the `linkedin-announce` CI job (`ci/linkedin-announce.sh`).
It creates a **DRAFT** LinkedIn post of the release notes on a stable semver
tag; a Page admin then reviews and publishes it in LinkedIn (four-eyes). This
file is intentionally **not** on the public site — keep it in the repo.

Do this once (plus a ~yearly token refresh). Everything is done as a **Company
Page admin**.

## 1. Create the LinkedIn app
1. https://www.linkedin.com/developers/apps → **Create app**. Associate it with
   the **Company Page** (required to post as the organization).
2. **Auth** tab → note **Client ID** and **Client Secret**.
3. Add an **Authorized redirect URL**, e.g. `https://localhost:8000/callback`
   (any URL you control — it only receives the auth `code`).
4. **Products** tab → request **Community Management API** (grants
   `w_organization_social` and, importantly, **refresh tokens**). Approval can
   take a bit. (For a *personal* profile instead: request **Share on LinkedIn**
   → scope `w_member_social`, and use a `urn:li:person:<id>` author.)

## 2. Get an authorization code (browser, as a Page admin)
Open this URL (one line), log in, approve:

```
https://www.linkedin.com/oauth/v2/authorization?response_type=code&client_id=<CLIENT_ID>&redirect_uri=<REDIRECT_URI>&scope=w_organization_social%20r_organization_social&state=xyz123
```

You are redirected to `<REDIRECT_URI>?code=<CODE>&state=xyz123`. Copy `<CODE>`
(valid ~30 min, single use).

## 3. Exchange the code for tokens
```sh
curl -X POST https://www.linkedin.com/oauth/v2/accessToken \
  --data-urlencode grant_type=authorization_code \
  --data-urlencode code=<CODE> \
  --data-urlencode redirect_uri=<REDIRECT_URI> \
  --data-urlencode client_id=<CLIENT_ID> \
  --data-urlencode client_secret=<CLIENT_SECRET>
```
The response has `access_token`, `expires_in` (~60 days), and
**`refresh_token`** with `refresh_token_expires_in` (~1 year). If there is **no**
`refresh_token`, the app is not enrolled for programmatic refresh — re-check the
Community Management API product in step 1.4.

## 4. Find the organization URN (the author)
```sh
curl -H "Authorization: Bearer <ACCESS_TOKEN>" \
  -H "LinkedIn-Version: 202401" -H "X-Restli-Protocol-Version: 2.0.0" \
  "https://api.linkedin.com/rest/organizationAcls?q=roleAssignee&role=ADMINISTRATOR&state=APPROVED"
```
Take the `organization` field → `urn:li:organization:<id>` = `LINKEDIN_AUTHOR_URN`.
(Personal profile alternative: `GET https://api.linkedin.com/v2/userinfo` with
scope `openid profile` → `sub` → `urn:li:person:<sub>`.)

## 5. Set the CI/CD variables
Project → **Settings → CI/CD → Variables**. Mark each **Masked** (and
**Protected** — release tags run on protected refs):

| Variable | Value |
|---|---|
| `LINKEDIN_AUTHOR_URN` | `urn:li:organization:<id>` (or `urn:li:person:<id>`) |
| `LINKEDIN_CLIENT_ID` | app Client ID |
| `LINKEDIN_CLIENT_SECRET` | app Client Secret |
| `LINKEDIN_REFRESH_TOKEN` | the `refresh_token` from step 3 |

Optional: `LINKEDIN_LIFECYCLE` (`DRAFT` default, or `PUBLISHED` to skip the human
gate), `LINKEDIN_API_VERSION` (YYYYMM, default `202401`), `SITE_URL`.

The job mints a fresh access token from the refresh token on every run, so the
short-lived access token never has to be stored.

## 6. Preview, then go live
- **Preview:** Pipelines → **Run pipeline** on a release tag with variable
  `LINKEDIN_DRY_RUN=1` → the `linkedin-announce` job prints the exact post +
  payload without calling LinkedIn.
- **Live:** on the next stable release the job creates a **draft**. Publish it
  from **LinkedIn Page → Admin tools → Drafts** (this is the four-eyes step).

## Maintenance & security
- The **refresh token expires (~1 year)** — repeat steps 2–3 to mint a new one
  and update `LINKEDIN_REFRESH_TOKEN` before it lapses.
- Treat the refresh token and client secret like passwords (masked + protected).
  Anyone holding them can post as the Page. Rotate if leaked.
- The job is `allow_failure` and skips when unconfigured, so it never blocks a
  release.
