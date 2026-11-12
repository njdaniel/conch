# Decision record: code-signing the Windows desktop app and its voice helper (F12, issue #901)

- **Status:** Proposed, 2026-11-12, **for Nick**. Nothing here is decided, bought or enrolled, and no workflow, configuration or code changes with it. A release is Tier-H ([ADR-000](../adr/ADR-000-charter.md)), and the signing account and certificate are his purchase ([windows-desktop.md §12](windows-desktop.md); not covered by his 2026-11-11 sign-off on #756).
- **Issues:** this note is #901 (F12 of [windows-desktop.md §14](windows-desktop.md)); epic #756; it blocks #780 (installer, signing, updates).
- **Wire shapes:** none. No schema, MCP tool or API changes.
- **Sources** were fetched on **2026-11-12** and are listed in §10; every price carries that date. A statement that was not read from a fetched page says **memory, not checked**, and §9 collects them. Where two sources disagree, both are given and Microsoft's own page is preferred for what Windows does.

## 0. The decision in one table

| # | Option | Cost (2026-11-12) | Who can get it | Publisher name Windows shows | First downloads: SmartScreen | Smart App Control, enforcing | In `release.yml` |
| --- | --- | --- | --- | --- | --- | --- | --- |
| 1 | **No signing** (technical preview) | 0 | anyone | none ("unknown publisher": memory, not checked) | warning, "Windows protected your PC"; the person must choose "Run anyway"; policy can remove that choice | **blocked, with no per-app way round** | a build job with no secrets |
| 2 | **Azure Artifact Signing**, Basic | USD 9.99 a month (USD 119.88 a year); 5,000 signatures a month | organisations in the listed countries; **individuals in the USA or Canada only** | the validated legal name (a person's, or the organisation's) | warning until reputation builds; the name is shown | allowed (signed by a trusted certificate) | OpenID Connect, no stored secret, no key to hold |
| 3 | **OV or IV certificate** from a public CA | about USD 129 to 700 a year for the certificate, plus a token (USD 379) or a cloud signer (from USD 180 a year) | OV: a registered legal entity. IV: a person | the validated legal name | the same as option 2 | allowed | a cloud signer's credentials as secrets; a USB token cannot be used by a hosted runner |
| 4 | **EV certificate** | about USD 349 to 970 a year, plus a token or a cloud signer (EV cloud signing from USD 900 a year at one CA) | a registered organisation | the organisation's name | **the same as option 3 since 2024**: no instant reputation | allowed | as option 3 |
| 5 | **SignPath Foundation** (free, open source) | 0 | open-source projects it accepts, at its discretion | **"SignPath Foundation"** | the same as option 2 (an OV-level certificate) | allowed | its GitHub action; one approval per release |

**Recommended** (§6): **a preview is not signed** (option 1), is labelled so, goes to named testers and carries the same checksums and attestation as the Linux `.deb`. **The first public Windows build is signed with Azure Artifact Signing** (option 2), if Nick is eligible, which he is as an individual only in the USA or Canada. If he is not eligible, the fallback is SignPath Foundation (option 5), then an IV certificate with a cloud signer (option 3). If he is eligible but does not want his legal name on every binary, SignPath Foundation is the only option here that avoids it without a legal entity. An EV certificate is not recommended at all.

## 1. What Windows does with a download

These are the facts the options are measured against. Two of them correct [windows-desktop.md §9.4](windows-desktop.md), which quoted Electron's page.

**SmartScreen** "evaluates two signals": publisher reputation ("Is the file signed? Is the signing certificate from a known, trusted publisher?") and file-hash reputation. "Even when signed, a newly created binary could still show a SmartScreen warning until its hash or publisher certificate accumulates sufficient evidence of positive reputation." Microsoft's table of first-download behaviour ([SmartScreen reputation for Windows app developers](https://learn.microsoft.com/en-us/windows/apps/package-and-deploy/smartscreen-reputation), updated 2026-08-17):

| Signature | First download |
| --- | --- |
| Valid certificate (OV or EV) | "Warning — app flagged as unrecognized until reputation accumulates; verified publisher name is displayed" |
| No signature | "Warning — 'Windows protected your PC'; User must choose 'Run anyway' before the app can run. Enterprise policy can prevent continuation entirely." |
| Self-signed | "Same behavior as no signature" |

How long reputation takes: "There is no exact threshold, but it can take several weeks and hundreds of clean installs from a wide audience." There is "no need (or mechanism) to manually submit a file for SmartScreen reputation review for consumer endpoints". What carries over between versions: "When a file is not signed, SmartScreen reputation must build for each new version of your files, starting with zero reputation. Reputation cannot transfer from previous versions unless both were signed using the same publisher identity." So signing does not remove the first warning; it puts a name in it, and it is the only way one release's standing can help the next. SmartScreen acts on files from the internet, not on files from "internal locations or network shares" ([SmartScreen overview](https://learn.microsoft.com/en-us/windows/security/operating-system-security/virus-and-threat-protection/microsoft-defender-smartscreen/), 2026-04-23).

**Smart App Control** (Windows 11) is stricter and is not a prompt. In enforcement mode "apps cannot be run unless they are recognized by Microsoft's app intelligence services, or they are signed with a trusted certificate"; "unknown, unsigned code [is] blocked by default" ([Smart App Control](https://learn.microsoft.com/en-us/windows/apps/develop/smart-app-control/overview), 2026-09-30). It "will block execution of unsigned files unless the file has a positive reputation", and its "signature checks apply to all executable files, not just those downloaded from the Internet" (the SmartScreen page above). For the person at the keyboard, "there is currently no way to bypass Smart App Control protection for individual apps": the alternatives Microsoft gives are turning the feature off or asking the developer to sign ([What is Smart App Control?](https://support.microsoft.com/en-us/topic/what-is-smart-app-control-285ea03d-fa88-4d56-882e-6698afdb7003), undated). It has three states, On, Evaluation and Off; evaluation mode decides by itself whether to enforce. For developers, "Artifact Signing is the preferred way to sign your app", and both RSA and ECC certificates are accepted ([Sign your app for Smart App Control compliance](https://learn.microsoft.com/en-us/windows/apps/develop/smart-app-control/code-signing-for-smart-app-control), 2026-09-25).

**EV no longer buys anything from SmartScreen.** "EV certificates no longer bypass SmartScreen. Years ago, signing files with an Extended Validation (EV) code signing certificate would result in positive SmartScreen reputation by default, but this behavior no longer exists. ... Paying a premium for EV solely to avoid SmartScreen warnings is no longer justified" (the SmartScreen page). The Trusted Root Program's requirements give the mechanism: "Starting February 2024, Microsoft will no longer accept or recognize EV Code Signing Certificates ... Beginning in August 2024, all EV Code Signing OIDs will be removed from existing roots in the Microsoft Trusted Root Program, and all Code Signing certificates will be treated equally" ([Program Requirements](https://learn.microsoft.com/en-us/security/trusted-root/program-requirements) §3.D.3, 2026-04-28; the page says it is superseded by a repository that was not read).

**Corrections to §9.4.** That section quoted Electron: Windows "requires software to be signed with an 'extended validation' certificate", and Artifact Signing "gets rid of SmartScreen warnings". [Electron's page](https://www.electronjs.org/docs/latest/tutorial/code-signing) still says both on 2026-11-12, and adds that a cheaper certificate is treated "as completely unsigned". Microsoft's pages above say otherwise on all three: an OV certificate is a valid certificate whose publisher name is shown; EV gives nothing more; and Artifact Signing "does **not** provide instant SmartScreen trust" ([Code signing options](https://learn.microsoft.com/en-us/windows/apps/package-and-deploy/code-signing-options), updated 2026-11-10). electron-builder's own Windows page (v26) is stale the same way: it says EV certificates work without warnings. This record follows Microsoft.

## 2. What is signed

The same for every option that signs. [windows-desktop.md §9.4](windows-desktop.md) already says "everything shipped is signed: installer, uninstaller, the app executable, and `conch-voice.exe`"; this adds the libraries and the order.

| File | Signed | How, with the pinned electron-builder 26.17.0 (its source, `app-builder-lib/out/winPackager.js` and `targets/nsis/NsisTarget.js`, read locally) |
| --- | --- | --- |
| `conch-desktop.exe`, the app (Electron's executable with our name, icon and fuses) | yes | electron-builder edits its resources and then signs it (`signAndEditResources`). It must be signed after the fuses are set and never changed afterwards: "avoid modifying files after signing" (Microsoft) |
| `conch-voice.exe`, the helper | yes | **not by default.** On Linux the helper is copied into `resources/` after packaging (`scripts/desktop-build.sh`), where electron-builder never sees it. On Windows it is signed by its own step before the installer is assembled, or bundled through `extraResources`, whose `.exe` files electron-builder does sign (`createTransformerForExtraFiles`). #780 chooses; either way the release fails if it is unsigned (§4) |
| The NSIS installer | yes, last | electron-builder signs it after building it (`signIf(installerPath)`) |
| The uninstaller | yes | electron-builder builds it, signs it and embeds it while it builds the installer. **It cannot be signed afterwards**, so signing happens inside the packaging step, not as a step on the finished installer (§4.3) |
| `elevate.exe` | not shipped, proposed | electron-builder packs it by default (`packElevateHelper`, default true: "required for electron-updater if per-machine installer used") and signs it. Ours is per-user with manual updates (§9.3, §9.5), so it can be left out; if it stays, it is one more signed file |
| DLLs beside the app (Chromium's and Electron's: `ffmpeg.dll`, `libEGL.dll`, `libGLESv2.dll`, `d3dcompiler_47.dll`, `vulkan-1.dll`, `vk_swiftshader.dll` and others: the list is memory, not checked) | **yes, proposed**, where the file carries no valid signature already | electron-builder signs only names ending `.exe` unless `signExts` lists more (`shouldSignFile`), so DLLs are unsigned by default. Smart App Control's check covers "all executable files", so an unsigned library is a risk until E12 shows otherwise. Whether Electron's release already signs any of them is memory, not checked: #780 lists every PE file in the unpacked app with its signature state before and after |
| Any DLL `conch-voice.exe` needs (the MSVC runtime, if it is not linked statically) | yes, if we ship it | not known until the Windows build is packaged (#780) |
| `app.asar`, JSON, images | no | not executable; covered by the installer's signature and by `enableEmbeddedAsarIntegrityValidation`, which is already on |

Every signature is SHA-256 with an RFC 3161 timestamp, so that it stays valid after the certificate expires. A release makes a few tens of signatures at most (the exact count is #780's to measure); every option's quota is compared with that.

## 3. The options

### 3.1 No signing: a technical preview

**What a tester sees.** A SmartScreen warning titled "Windows protected your PC" on the downloaded installer; they "must choose 'Run anyway' before the app can run", and "enterprise policy can prevent continuation entirely" (§1). That the button sits behind a "More info" link, and that the browser may first ask them to keep an uncommonly downloaded file, is memory, not checked. Every new build starts again from no reputation.

**On a PC where Smart App Control is enforcing, an unsigned build does not run at all**, and there is no button. The only way round is to turn the feature off, which this project **never asks anyone to do** (#780; §9.4). Such a tester waits for a signed build. Which Windows 11 installations enforce it was not established: that a clean install starts in evaluation mode is memory, not checked, and Microsoft's support page says recent updates let it be turned on without a clean install.

**What we do instead of a signature:** the release notes say the build is an unsigned technical preview, what Windows will show, and how to check the file first (`sha256sum`, and `gh attestation verify` with the workflow and the tag, as for the `.deb`). Microsoft's own advice for early adopters is the same: tell them they "may see a SmartScreen prompt on first download, and that they should only proceed after verifying the publisher and confirming they trust the download source". No instruction to disable SmartScreen, Smart App Control or antivirus is ever given.

**Cost:** nothing. **Account:** none. **Loss:** nothing to lose.

### 3.2 Azure Artifact Signing (formerly Trusted Signing)

Microsoft's managed signing service, and its "recommended code signing service for non-Store distribution". The key never leaves Microsoft's hardware ("FIPS 140-3 Level 3"); "the Authenticode certificate that's used for signing with the profile is never given to you".

**Eligibility** ([Quickstart](https://learn.microsoft.com/en-us/azure/artifact-signing/quickstart), updated 2026-10-08): "Public Trust certificates are available to organizations in the United States, Canada, the European Union, the United Kingdom, Australia, New Zealand, Japan, South Korea, Singapore, Switzerland, Norway, and Israel. **Individual developers must be located in the United States or Canada.**" Microsoft's comparison page, updated 2026-11-10, gives a shorter list for organisations (USA, Canada, EU, UK) and the same rule for individuals. No years-in-business requirement is on either page today (a search result quoting an older Electron page mentioned three years of business history; it is not on Microsoft's pages now). It needs "a paid Azure subscription": free, trial and sponsored subscriptions are refused ([FAQ](https://learn.microsoft.com/en-us/azure/artifact-signing/faq), updated 2026-10-27).

**Identity validation.**
- *Individual:* the details come from the Azure billing account, which "must have an Account Type of Individual", and "the legal name and sold-to address on the billing account must match the information on the government-issued ID". The check is done in the Azure portal only, through Microsoft Entra Verified ID with a partner (AU10TIX in the walkthrough): a phone with Microsoft Authenticator, a government photo ID, a face check, and sometimes a proof of address (a utility bill or bank statement). The portal updates "a couple of minutes" after it succeeds; Microsoft's comparison page says to "plan for a few business days".
- *Organisation:* legal name, a website, two email addresses on the organisation's own domain, a business identifier and address, and then the same individual check for the person representing it. "Processing ... takes from 1 to 20 business days".
- The certificate's subject is the validated name and nothing else: "you can't use a custom Common Name (CN) or a custom Organization (O)". For an individual it also shows city, state or province and country; "your email address and street address aren't included". **So an individual's certificate puts Nick's legal name on every signed file and in every warning.** The product name "Conch" cannot be the publisher unless a legal entity of that name is validated.
- Validations expire (the period is not stated); reminders start 60 days before, and without renewal "all signing processes ... stop".

**Cost.** Basic: **USD 9.99 a month**, 5,000 signatures a month, one certificate profile of each type. Premium: USD 99.99 a month, 100,000 signatures, ten profiles. Beyond the quota: USD 0.005 a signature. (Azure Retail Prices API, service "Trusted Signing", queried 2026-11-12, prices effective since 2024-06-01; Microsoft's comparison page says "starts at $9.99/month". The public pricing page renders its numbers by script and showed none to this session.) "The pricing isn't calculated on a pro rata basis": the full month is charged from the day the account is created, used or not, so the account is created when the validation is about to be done, and deleted if validation fails ("so that you aren't billed for unused resources"). That a pay-as-you-go subscription has no fee of its own is memory, not checked.

**What it signs.** "All file types that SignTool supports": `.exe`, `.dll`, the installer. Its certificates "are renewed daily and are valid for only 72 hours", so "time stamping is critical"; the timestamp authority is `http://timestamp.acs.microsoft.com`.

**Reputation.** Yes, but not at once. "SmartScreen reputation builds up automatically. The prompt stops appearing once the file hash has sufficient download history" (FAQ). It "does **not** provide instant SmartScreen trust, but signing consecutive releases with a consistent publisher/signing identity lets publisher reputation build over time, so later releases can inherit trust", and OV certificates are "functionally equivalent to Azure Artifact Signing for SmartScreen purposes" (comparison page). The service "doesn't issue Extended Validation (EV) certificates" and has "no plan to". How publisher reputation follows a certificate that changes daily is not stated anywhere read; each profile has a durable identity (a custom EKU, `1.3.6.1.4.1.311.97.…`) that "stays the same" for the profile's life, and "if you delete the certificate profile and create a new one, Artifact Signing assigns a new EKU". **The profile is therefore created once and never deleted.**

**One thing to watch.** "Artifact Signing code-signing certificate authorities (CAs) are scheduled for removal from the Common CA Database (CCADB). Although Microsoft no longer includes these CAs in the Trusted Root Program, they remain on the Windows Platform Trust List. Windows continues to trust certificates that chain to these CAs" ([certificate management](https://learn.microsoft.com/en-us/azure/artifact-signing/concept-certificate-management), 2026-09-30). The Smart App Control overview speaks of certificates from "a certificate authority (CA) within the Trusted Root Program", while its signing page names Artifact Signing as preferred. Read together they mean Windows trusts it; E12 confirms it on a machine where Smart App Control enforces.

**In GitHub Actions.** Three routes on a Windows runner (Electron's page also names `jsign` for signing from Linux or macOS; it is not needed here, since the helper and the installer are built on Windows):
1. The official action, [`azure/artifact-signing-action`](https://github.com/Azure/artifact-signing-action) (MIT; v2.0.0, 2026-05-14; runs on `windows-2022` and `windows-2025`, not on ARM). It signs a list or a folder of files and recommends OpenID Connect: `azure/login` exchanges the job's GitHub token for an Azure one, so **no secret is stored**. The identity it logs in as needs the role "Artifact Signing Certificate Profile Signer". It downloads its dependencies (SignTool, the dlib, .NET) when it runs; its cache is off by default "because GitHub Actions caches are not a security boundary".
2. `signtool` with the dlib: Windows SDK SignTool 10.0.22621.755 or later (Microsoft's page misprints it as 10.0.2261.755), the .NET 8 runtime and the NuGet package `Microsoft.ArtifactSigning.Client`, with a `metadata.json` naming the endpoint, the account and the profile: `signtool sign /fd SHA256 /tr http://timestamp.acs.microsoft.com /td SHA256 /dlib …\Azure.CodeSigning.Dlib.dll /dmdf metadata.json <file>` ([signing integrations](https://learn.microsoft.com/en-us/azure/artifact-signing/how-to-signing-integrations)). Each piece can be pinned by version and hash, which suits a release built without caches.
3. electron-builder's `win.azureSignOptions` (`publisherName`, `endpoint`, `certificateProfileName`, `codeSigningAccountName`). In 26.17.0 this installs the PowerShell module `TrustedSigning` from the PowerShell Gallery at build time, **unpinned** (`-MinimumVersion 0.5.0`), and calls `Invoke-TrustedSigning` for each file (`windowsSignAzureManager.js`, read locally). Its documentation uses a client secret in the environment (`AZURE_TENANT_ID`, `AZURE_CLIENT_ID`, `AZURE_CLIENT_SECRET`); whether it works from an `azure/login` OpenID Connect session was not tried.

**Account and loss.** The Azure tenant, the subscription and its billing account are Nick's, under his Microsoft account; the validated identity is his or his organisation's. There is no key or token to lose and nothing to export. What can be lost is access:
- the Azure account or its payment lapses, or the validation expires: signing stops; files already signed stay valid (deleting even the whole account "doesn't affect the certificates that were already used to sign your files");
- the resources "can't be migrated across subscriptions or tenants": moving means creating them again, with a new profile and a new EKU;
- misuse: Microsoft "may revoke certificate(s) and suspend access", and a revocation can be as narrow as one day's certificate;
- eligibility or the service changes: fall back to §3.3; whether reputation follows the same legal name to another CA's certificate is not stated.

### 3.3 An OV (or IV) certificate from a public CA

**Who.** Organisation Validation needs a registered legal entity. A person without one buys Individual Validation, which puts their own name on the certificate (SSL.com: "Personal name on every installer. No business docs required").

**The 2023 rule.** The CA/Browser Forum's [Code Signing Baseline Requirements](https://cabforum.org/working-groups/code-signing/requirements/) (v3.11.0, 2026-06-16), §6.2.7.4.1: from 1 June 2023 the subscriber's key must be in a hardware crypto module "certified as conforming to at least FIPS 140-2 Level 2", by one of three routes: the subscriber's own hardware module (a USB token), "a cloud-base key generation and protection solution", or a signing service. **A `.pfx` file in a secret is no longer possible.** And §6.3.2: "for Code Signing Certificates issued on or after March 1st, 2026, the validity period MUST NOT exceed 460 days", so a "three-year" purchase is now a certificate reissued about every fifteen months (Sectigo's multi-year plans send a new certificate and a new token each year).

**Cost a year** (list prices on the CA's own page, 2026-11-12, one-year term unless said):

| CA | Certificate | Key storage |
| --- | --- | --- |
| SSL.com | OV USD 129 (IV the same, from a search result, its page not opened) | YubiKey token USD 379 each; **eSigner cloud** tier 1 USD 180 a year or USD 20 a month for 20 signings a month, USD 1 each beyond; attestation of your own cloud HSM USD 75 (Azure or Google) |
| Certum | Standard Code Signing "in the Cloud" from EUR 209; card and reader set from EUR 169 | included (SimplySign, or the card) |
| Sectigo | "starts at $536.25 per year" on a five-year plan; its page shows no other price | a token with each certificate; your own HSM or Google Cloud KMS possible |
| DigiCert | USD 696 a year (the page shows a second price, USD 972, and does not say which is OV and which EV) | KeyLocker cloud, a token or your own HSM; no price shown |

Microsoft's own range is "typically $150–300/year". With a cloud signer the realistic first-year total is about **USD 310** (SSL.com IV or OV with eSigner tier 1) to about USD 700 and more.

**SmartScreen and Smart App Control:** the same as option 2 (§1). A renewal is a new certificate; whether its reputation starts again is disputed (SSL.com says renewals "typically build reputation faster"; a 2026 Microsoft Q&A thread is titled "SmartScreen Reputation Reset Following EV Certificate Renewal"; neither was verified).

**In CI.** A USB token is in Nick's hands, not on a hosted runner: it means signing by hand on his machine, or a self-hosted runner, for every release. A cloud signer works from a hosted runner with credentials stored as secrets: for DigiCert KeyLocker, five (`SM_HOST`, `SM_API_KEY`, `SM_CLIENT_CERT_FILE_B64`, `SM_CLIENT_CERT_PASSWORD`, `SM_CODE_SIGNING_CERT_SHA1_HASH`) and its `smctl` tool ([DigiCert](https://docs.digicert.com/en/digicert-keylocker/ci-cd-integrations-and-deployment-pipelines/plugins/github/install-client-tools-for-standard-keypair-signing-on-github.html), which also says the GitHub action it documents reached end of life in February 2026 and names its replacement). eSigner's quota is small: 20 signings a month is about one release with its libraries. Certum's cloud needs a one-time code from its phone app; unattended use is not described on its page. electron-builder reaches any of them through a custom `sign` function, which is our code running with the signing identity.

**Account and loss.** The CA account and the signer's account are Nick's; the secrets live in a GitHub environment (§4.2). A lost or stolen token, or leaked signer credentials, mean revocation and a reissue (the CA's fee and delay were not checked), and Microsoft warns that "changing your signing certificate affects the publisher trust signal". Signatures already made stay valid unless the certificate is revoked back past their date.

### 3.4 An EV certificate

**Cost.** SSL.com USD 349 a year, plus a token (USD 379) or eSigner's EV tier (USD 900 a year, 120 signings; USD 10 each beyond); Certum EV "in the Cloud" from EUR 379; DigiCert USD 972 or 696 (unlabelled, above). Microsoft: "$400+/year". An organisation is required (SSL.com sells a separate "Sole Proprietor EV").

**What SmartScreen does differently today: nothing** (§1). Microsoft: EV "previously bypassed SmartScreen entirely on first download ... That behavior was removed in 2024. EV-signed files now go through the same reputation-building process as OV certificates", and "paying the EV premium ... solely to avoid SmartScreen warnings is **no longer justified**". Sectigo and SSL.com now say the same on their own sales pages. EV is still required for kernel-mode drivers, which we do not ship (§5 of the Windows design: no driver). CI, account and loss are as §3.3, at a higher price.

### 3.5 Free and open-source signing

**SignPath Foundation.** It signs open-source projects' releases with **its own** certificate, free ("For OSS projects, our services are free of charge"), the key in its HSM. Microsoft's comparison page lists it: "free code signing for qualifying open-source projects ... OV-level certificate signing through a managed pipeline". Its conditions ([terms](https://signpath.org/terms), marked a draft; as summarised from the page):
- an OSI-approved licence with no commercial dual-licensing (AGPL-3.0 qualifies), no proprietary components, actively maintained, and **already released** in the form to be signed (so an unsigned preview comes first);
- **the publisher is "SignPath Foundation"**, not the project or its owner; it may "pause or terminate a subscription without notice" and revoke, and acceptance is at its discretion;
- a "Code signing policy" page on the project's site naming the team and its roles (authors, reviewers, approvers), with a privacy policy; multi-factor authentication for everyone; **every release approved by hand** for signing;
- artifacts "built from source code in a verifiable way": for open source, every job leading to the request on GitHub-hosted runners, the unsigned artifact uploaded with `actions/upload-artifact`, then `signpath/github-action-submit-signing-request` with an API token as a secret ([SignPath's GitHub page](https://docs.signpath.io/trusted-build-systems/github));
- "upstream OSS binaries must not be signed under your subscription, though they may be included in signed packages". **That needs their answer before applying**: `conch-desktop.exe` is Electron's own executable with our resources, and the DLLs are Chromium's. None of the roughly 330 listed projects could be identified as an Electron app from the list (Mumble, a voice client, is among them).

It fits the release process (the approval is Nick's, like the tag) and hides no key in our hands. Against it: the name on the warning is someone else's, the signer is theirs to withdraw, and the uninstaller constraint of §2 has to be solved through their pipeline (that one request can sign an installer and the files inside it, by an "artifact configuration", is memory, not checked).

**Certum Open Source Code Signing.** A cheap certificate for an individual open-source developer, not a free one: "in the Cloud" from EUR 49, a card-and-reader set from EUR 69 (2026-11-12; the cloud product's page said "out of stock" on that day). The subject is the person's name "prefixed with 'Open Source Developer'"; the cloud version signs through the SimplySign phone and desktop apps, with a limit of 5,000 signatures a month. Whether it can sign unattended on a hosted runner, and its validity period, are not on the pages read.

### 3.6 Not options

- **A self-signed certificate:** "Same behavior as no signature" for the public (Microsoft).
- **The Microsoft Store:** an MSIX published there is re-signed by Microsoft, free, and never warned about; MSIX and the Store are deferred by #756, and reopening that is a roadmap decision, not this one. A Win32 installer submitted to the Store must still be signed by us.
- **The GitHub attestation alone:** it proves which workflow built a file at which tag, to anyone who runs `gh attestation verify`. Windows does not read it. It is kept alongside a signature (§4.4), never instead of one.

## 4. How it fits `release.yml`

The shape is the one PR #1634 (`issue-1598/web-engineer`, open) gives the Linux desktop: `release` (gates and Go binaries), `desktop-deb`, `attest`, `publish`, on a `v*` tag that only Nick pushes. This section says what a Windows job needs; the workflow itself is #780's, and any change to it is Nick's to merge.

### 4.1 A job beside the `.deb`'s

`desktop-windows` (the name is #780's) runs on a pinned Windows runner image beside `desktop-deb`, under the same rules: the tag guard first (`scripts/desktop-release-guard.sh`), no restored caches for anything that ends up in the release, the pinned toolchain (`scripts/voice-pins.sh`, the desktop's Node), `conch-voice.exe` built `--release --locked` and reporting the tag's version. It uploads one installer; `attest` and `publish` wait for it.

- **Preview (option 1):** that job and nothing more. It has no secrets and `permissions: contents: read`.
- **Signed (options 2 to 5):** the same job produces the signed installer, with the gate of §4.2 added.

### 4.2 Secrets, and never signing on a pull request

- **Only `release.yml` signs, and it runs only on a `v*` tag push.** `ci.yml` builds the installer unsigned on pull requests to test packaging; it never has `id-token: write` and is never given a signing secret. GitHub does not pass secrets to workflows "triggered from a forked repository" in any case.
- **The signing job names a GitHub environment** (say `windows-signing`) whose deployment rule allows only `v*` tags. "Secrets stored in an environment are only available to workflow jobs that reference the environment"; with required reviewers, "a job cannot access environment secrets until one of the required reviewers approves it". Both features are available to this repository because it is public. Whether Nick also wants to be a required reviewer (a second click after pushing the tag) is his choice.
- **Option 2 stores no secret at all.** The Azure identity trusts one subject, `repo:njdaniel/conch:environment:windows-signing`; GitHub puts the environment in the token's subject only "when the job references an environment", and a pull-request job's subject is `repo:…:pull_request`. A token from any other job, branch or fork cannot be exchanged. The three values `azure/login` takes (client, tenant and subscription ids) are identifiers, kept as environment secrets by convention. The identity holds one role, Certificate Profile Signer, on the one certificate profile.
- **Options 3 and 5 store real secrets** in that environment (a cloud signer's API key and client certificate; SignPath's API token). Each can sign as us until rotated.
- **No agent holds signing material.** Microsoft's comparison page says it directly: "Don't let an AI agent create, export, or commit release certificates or signing credentials." The account, the validation, the role assignment and the environment are set up by Nick; an agent's part is the workflow PR, which contains names and no credentials.

```yaml
# The gate, not the job: what the signing job adds to desktop-deb's shape.
environment: windows-signing   # deployment rule: tags matching v* only
permissions:
  contents: read
  id-token: write              # option 2 only: the token azure/login exchanges
```

### 4.3 Where the signature is made

Because the uninstaller and the inner executables are sealed inside the installer (§2), signing is part of packaging. Two ways to wire option 2, for #780 to choose:

| Wiring | For | Against |
| --- | --- | --- |
| electron-builder's `azureSignOptions`, with `forceCodeSigning: true` | one configuration block; it signs the app, the uninstaller and the installer in the right order | installs an unpinned PowerShell module in the release job; documented with a client secret, OpenID Connect untried; `conch-voice.exe` and DLLs need `signExts` or `extraResources` |
| electron-builder's custom `sign` function calling pinned `signtool` and the dlib (route 2 of §3.2) | every tool pinned by hash, as the `.deb` job pins its toolchain; works after `azure/login` | a small script of ours runs with the signing identity and replaces electron-builder's own signing |

The official action (route 1) fits only files that exist before packaging (`conch-voice.exe`) or the finished installer; alone it cannot reach the uninstaller.

**A cost to state plainly:** the job that packages also runs `npm ci` and `cargo build`. With `id-token: write` on that job, any code in it could ask for the token. The `.deb` job answers the same worry with locked dependencies and no caches; splitting build and signing into two jobs is cleaner and is made awkward by the uninstaller. #780 decides, and the PR gets a `security-reviewer` pass.

**The check that makes it safe to rely on:** before upload, the job verifies every portable-executable file in the unpacked app (`conch-desktop.exe`, `conch-voice.exe`, every DLL) and the outer installer (`signtool verify /pa /v`, the expected subject name, a timestamp present), and fails the release on any unsigned or unexpected one. The NSIS uninstaller exists only inside the packed installer, so it is verified inside the signing hook when electron-builder signs it, or by extracting the installer's payload (for example with 7-Zip) and verifying it there; #780 chooses which. Left alone, electron-builder with no certificate configured logs "signing is skipped" and succeeds (`windowsSignToolManager.js`); `forceCodeSigning` turns that into an error, and the check above does not depend on it.

### 4.4 Attestation alongside

Unchanged in kind. Signing changes the file's bytes, so the order is: sign, then `attest` writes `checksums.txt` over the signed installer and attests it, then `publish` verifies each attestation and creates the release. `attest` asserts the exact file set, so its list gains one name. The release notes tell a Windows tester both checks: the publisher Windows shows (or, for a preview, that there is none) and `gh attestation verify <file> --repo njdaniel/conch --signer-workflow njdaniel/conch/.github/workflows/release.yml --source-ref refs/tags/<tag>`.

### 4.5 New pieces Nick signs

There is no machine-checked allowlist for GitHub Actions; as with `actions/attest-build-provenance`, each new action is a dependency line he signs in the #780 PR, pinned by full commit SHA: for option 2, `azure/login` and, if used, `azure/artifact-signing-action`; for option 5, SignPath's action. No `go.mod`, Cargo or npm dependency is added by any option.

## 5. Who owns the signer, and what happens if it is lost

| Option | Owner | What can be lost | Then |
| --- | --- | --- | --- |
| 1 None | nobody | nothing | nothing |
| 2 Artifact Signing | Nick's Azure tenant and subscription; the certificate is Microsoft's to issue, in his validated name | access to the account; the validation's renewal; the profile, if deleted | no new signatures until restored; old ones stay valid; a recreated profile is a new identity to pinned policies, and its effect on reputation is not stated |
| 3 OV or IV | Nick's CA account; a token in his hands, or a cloud signer's credentials in a GitHub environment | the token; the credentials (which can sign as us until revoked); the yearly reissue | revoke, reissue at the CA's price and pace; "changing your signing certificate affects the publisher trust signal" |
| 4 EV | as 3 | as 3 | as 3 |
| 5 SignPath | **SignPath Foundation**; we hold an API token | their acceptance, at any time and "without notice" | another signer and another publisher name; reputation starts again under it |

In every case the already published files keep working: a timestamped signature outlives its certificate unless that certificate is revoked back past the signing date.

## 6. Recommendation

**For a preview: do not sign (option 1).** It costs nothing, needs no account and waits on nobody. It is honest about what it is: a technical preview for named testers, labelled unsigned in the release notes, with the checksum and the attestation as the things to check, and with a plain statement that Windows will warn and that a PC with Smart App Control on will refuse it. It also satisfies SignPath's "already released" condition if that route is ever taken. Its reach is the limit: it is not something to hand to people who were not told what to expect.

**For the first public Windows build: Azure Artifact Signing, Basic (option 2), on a Public Trust profile, signed in `release.yml` through OpenID Connect behind a tag-only environment.** The reasons, in order:
1. It is the signer Microsoft recommends for this exact case, and the one its Smart App Control page prefers.
2. SmartScreen treats it as it treats OV and EV, so the dearer options buy nothing visible.
3. There is no key, token or secret to hold, leak or lose: nothing in GitHub can sign unless a `v*` tag was pushed.
4. At USD 119.88 a year it is the cheapest signer found that is Nick's own, is not at a third party's discretion and is documented to work unattended from a hosted runner. (Certum's Open Source certificate costs less; its unattended use is not documented and it was out of stock.)

It does not make the first warning go away, and nothing on sale does: the public build's notes still say a new app may be flagged until enough people have installed it.

**The condition.** Nick must be eligible: as an individual, located in the USA or Canada, with a government ID whose name and address match his Azure billing account. And he must accept that the publisher shown is that legal name, with his city, state or province and country in the certificate.

**If he is not eligible, or does not want his name on the binaries:** apply to SignPath Foundation (option 5), after asking them the upstream-binary question; if that is refused or the answer is no, an IV certificate with a cloud signer (option 3, about USD 310 a year at SSL.com, with its 20-signings-a-month quota in mind; it shows his legal name too, so it answers ineligibility, not the name). An organisation's name on the certificate needs a legal entity under any option.

**Not recommended:** EV (option 4), at any price. Enrolling early only to "build reputation" on previews is also not worth it: a handful of testers is not the "hundreds of clean installs from a wide audience" reputation needs. What is worth starting early is the identity validation, because it is the one step with a waiting time; once the profile exists, previews are signed with it too, at no extra cost, which also rehearses the release job.

## 7. What Nick must do

**For a preview (now): nothing to buy or enrol in.** One decision: accept an unsigned, labelled, attested preview for named testers.

**For the first public build (option 2), in this order.** Steps 1 to 8 are his own, in his own accounts; none can be done by an agent.
1. **Confirm eligibility:** an individual located in the USA or Canada (or an organisation in a listed country), with a government photo ID, and a phone for Microsoft Authenticator.
2. **Choose the name:** individual (his legal name; city, state or province and country shown; street and email not) or an organisation (a legal entity, its business identifier, a website and mailboxes on its own domain, and 1 to 20 business days).
3. **An Azure account:** a Microsoft account, a tenant and a **paid pay-as-you-go subscription** (a payment card) whose billing account is of type Individual, with the legal name and sold-to address exactly as on the ID.
4. **Create the Artifact Signing account:** register the `Microsoft.CodeSigning` resource provider; create the account with the **Basic** SKU in a supported Azure region (for example East US), matching the endpoint recorded in step 9. **The USD 9.99 a month starts here**, not pro-rated.
5. **Validate the identity:** give himself the role "Artifact Signing Identity Verifier"; create a Public identity validation; complete the Verified ID check (ID, face check, possibly a proof of address). If it fails for good, delete the account to stop the charge.
6. **Create one Public Trust certificate profile,** and never delete it.
7. **Create the release identity:** an Entra application (or a user-assigned managed identity) with a federated credential for the subject `repo:njdaniel/conch:environment:windows-signing`, and the role "Artifact Signing Certificate Profile Signer" on that profile only. No client secret.
8. **In GitHub:** create the environment `windows-signing`, allow only `v*` tags, add himself as a required reviewer if he wants the second click, and store the client, tenant and subscription ids there.
9. **Record in the repository** (not secret): the account name, the region endpoint, the profile name and the exact publisher name, for #780's configuration.
10. **Sign off in #780's PR:** the new actions as dependency lines, and the `release.yml` change.
11. **Keep it alive:** USD 9.99 a month (USD 119.88 a year); renew the identity validation when the reminders come; multi-factor authentication and recovery on the Microsoft account.

**If option 5 instead:** ask SignPath whether an Electron app's executable and Chromium's DLLs may be signed; publish an unsigned Windows release first; add a "Code signing policy" page and a privacy statement to the project; apply at signpath.org; then approve each release's signing request. Cost: none.

**If option 3 instead:** buy an IV (or OV) certificate and a cloud signing plan from one CA, complete its identity check, and place the signer's credentials in the `windows-signing` environment. Cost: from about USD 310 a year.

## 8. Open questions and experiments (for #780 and the hardware run, #903)

- **E12 gains three rows:** the unsigned preview and the signed build, each on a PC with Smart App Control Off, in Evaluation and On; the exact text of each dialog; and the same after a second release signed by the same identity.
- **The PE inventory:** every `.exe` and `.dll` in the unpacked app, with its signer before and after our signing; which of Electron's files arrive signed.
- **The wiring of §4.3,** including whether `Invoke-TrustedSigning` under electron-builder works from an OpenID Connect login, and whether build and signing can be separate jobs.
- **SignPath's answer** on upstream binaries, only if option 5 is wanted.
- **The capability matrix** ([windows-desktop-capabilities.md](../reviews/windows-desktop-capabilities.md), row D3) still names "EV with cloud HSM" as the alternative; it should point here once this is decided.

## 9. Not verified (memory, not checked, or read only in part)

- The "More info" step before "Run anyway", the browser's own download warning, and "unknown publisher" as the wording for an unsigned file.
- Which Windows 11 installations have Smart App Control enforcing; the support page contradicts itself on whether turning it on needs a reinstall.
- That a pay-as-you-go Azure subscription has no base fee; how long an identity validation lasts before renewal.
- How SmartScreen's publisher reputation follows Artifact Signing's daily certificates, a CA certificate's renewal, or a move between signers under the same legal name.
- The list of DLLs Electron ships, whether any is already signed, and whether `conch-voice.exe` needs a runtime DLL.
- Artifact Signing's public pricing page (numbers not rendered; the prices are from Azure's price API and Microsoft's comparison page); DigiCert's two prices (not labelled); SSL.com's IV price (a search result); CA fees and delays for reissue after a lost token.
- SignPath's terms are a draft and were read as a summary, not verbatim; its artifact-configuration mechanism and its application form were not read. Certum's unattended-signing ability and validity periods were not on the pages read.
- Whether the Trusted Root Program's current requirements (the repository that supersedes the page read) still carry the EV sentence; Microsoft's 2026 developer pages say the same thing independently.
- The Microsoft Q&A thread on reputation after an EV renewal (title only).
- Nothing was run on Windows, and nothing was signed.

## 10. Sources

All fetched 2026-11-12.

Microsoft Learn and Microsoft: [SmartScreen reputation for Windows app developers](https://learn.microsoft.com/en-us/windows/apps/package-and-deploy/smartscreen-reputation) (updated 2026-08-17) · [Code signing options for Windows app developers](https://learn.microsoft.com/en-us/windows/apps/package-and-deploy/code-signing-options) (updated 2026-11-10) · [Microsoft Defender SmartScreen overview](https://learn.microsoft.com/en-us/windows/security/operating-system-security/virus-and-threat-protection/microsoft-defender-smartscreen/) (2026-04-23) · [Smart App Control](https://learn.microsoft.com/en-us/windows/apps/develop/smart-app-control/overview) (2026-09-30) · [Sign your app for Smart App Control compliance](https://learn.microsoft.com/en-us/windows/apps/develop/smart-app-control/code-signing-for-smart-app-control) (2026-09-25) · [What is Smart App Control?](https://support.microsoft.com/en-us/topic/what-is-smart-app-control-285ea03d-fa88-4d56-882e-6698afdb7003) · [Trusted Root Program requirements](https://learn.microsoft.com/en-us/security/trusted-root/program-requirements) (2026-04-28) · Artifact Signing: [quickstart](https://learn.microsoft.com/en-us/azure/artifact-signing/quickstart) (updated 2026-10-08), [FAQ](https://learn.microsoft.com/en-us/azure/artifact-signing/faq) (updated 2026-10-27), [signing integrations](https://learn.microsoft.com/en-us/azure/artifact-signing/how-to-signing-integrations) (updated 2026-08-03), [certificate management](https://learn.microsoft.com/en-us/azure/artifact-signing/concept-certificate-management) (2026-09-30), [resources and roles](https://learn.microsoft.com/en-us/azure/artifact-signing/concept-resources-roles), [pricing page](https://azure.microsoft.com/en-us/pricing/details/artifact-signing/) (quotas only) · [Azure Retail Prices API](https://prices.azure.com/api/retail/prices) (service "Trusted Signing") · [Azure login from GitHub Actions by OpenID Connect](https://learn.microsoft.com/en-us/azure/developer/github/connect-from-azure-openid-connect).

GitHub: [`Azure/artifact-signing-action`](https://github.com/Azure/artifact-signing-action) (README, `docs/OIDC.md`, releases) · [OpenID Connect reference](https://docs.github.com/en/actions/reference/security/oidc) (subject claims) · [Deployments and environments](https://docs.github.com/en/actions/reference/workflows-and-actions/deployments-and-environments) · [Using secrets in GitHub Actions](https://docs.github.com/en/actions/security-for-github-actions/security-guides/using-secrets-in-github-actions).

CA/Browser Forum: [Code Signing Baseline Requirements](https://cabforum.org/working-groups/code-signing/requirements/) v3.11.0 (§6.2.7.4.1, §6.2.7.4.2, §6.3.2).

Certificate authorities (prices): SSL.com [OV](https://www.ssl.com/products/software-integrity/code-signing/ov/), [EV](https://www.ssl.com/products/software-integrity/code-signing/ev/), [code signing overview](https://www.ssl.com/certificates/code-signing/), [eSigner pricing](https://www.ssl.com/guide/esigner-pricing-for-code-signing/) (updated 2026-03-25) · Certum [code signing shop](https://shop.certum.eu/code-signing.html), [Open Source set](https://shop.certum.eu/open-source-code-signing.html), [Open Source in the Cloud](https://shop.certum.eu/open-source-code-signing-on-simplysign.html) · [Sectigo code signing](https://www.sectigo.com/ssl-certificates-tls/code-signing) · [DigiCert code signing](https://www.digicert.com/signing/code-signing-certificates), [KeyLocker on GitHub](https://docs.digicert.com/en/digicert-keylocker/ci-cd-integrations-and-deployment-pipelines/plugins/github/install-client-tools-for-standard-keypair-signing-on-github.html).

SignPath: [signpath.org](https://signpath.org/), [terms](https://signpath.org/terms), [projects](https://signpath.org/projects), [GitHub integration](https://docs.signpath.io/trusted-build-systems/github).

Electron and electron-builder: [Electron code signing](https://www.electronjs.org/docs/latest/tutorial/code-signing) · [electron-builder v26, Windows code signing](https://www.electron.build/v26/docs/features/code-signing/code-signing-win) · `app-builder-lib` 26.17.0 as installed (`out/winPackager.js`, `out/codeSign/windowsSignAzureManager.js`, `out/codeSign/windowsCodeSign.js`, `out/targets/nsis/NsisTarget.js`, `out/targets/nsis/nsisUtil.js`, `out/options/winOptions.d.ts`).

Repository: `.github/workflows/release.yml` on `main` and on `origin/issue-1598/web-engineer` (PR #1634) · `desktop/build/electron-builder.config.json` · `scripts/desktop-build.sh` · [windows-desktop.md](windows-desktop.md) §9, §12, §14 · [linux-desktop.md](linux-desktop.md) (Q-D6-4, Q-D6-7) · [macos-desktop.md](macos-desktop.md) §12.2 · issues #756, #780, #901.
