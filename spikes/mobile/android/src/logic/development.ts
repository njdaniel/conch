// The development marking (M-R1): a server reached over plain http:// is a
// development setup and the connection is not encrypted. The marking is derived
// from the address the client actually uses (ApiClient.development), never from
// the build type; it changes nothing about which addresses are accepted.

// The visible line on Channels and the voice screen.
export const developmentLine = 'Development server: this connection is not encrypted (plain http).';

// The state line on Diagnostics.
export const developmentState = 'server development';

// isDevelopmentServer: the address's scheme is plain http, however many
// slashes follow it: Android's URL parser (OkHttp) reads http:/host and
// http:host as http too, so the marking keys on the scheme alone. The scheme
// is not case sensitive. An https:, scheme-less or empty address is not one.
export function isDevelopmentServer(origin: string): boolean {
  return /^http:/i.test(origin.trim());
}

// developmentNotice is what a screen shows for the address in use: the visible
// line (Channels, voice) or the state line (Diagnostics), or null when the
// server is not a development one.
export function developmentNotice(origin: string, screen: 'channels' | 'voice' | 'diagnostics'): string | null {
  if (!isDevelopmentServer(origin)) return null;
  return screen === 'diagnostics' ? developmentState : developmentLine;
}
