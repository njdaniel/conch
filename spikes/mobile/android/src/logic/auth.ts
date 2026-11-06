// What the app does when the server says the credential is no good (design
// section 1.2; #1109: Credential and API use).
//
// A 401 from whoami or from the ticket route means the credential is dead:
// media is torn down first (microphone, camera, claim), and only then does the
// app show signed out. A 1008 close of a socket means the server refused the
// socket, not the credential: the app revalidates with whoami and acts on that
// answer, and keeps media as it is until then.
export type AuthSignal = 'whoami_401' | 'ticket_401' | 'socket_closed_1008';

export type AuthStep = 'tear_down_media' | 'show_signed_out' | 'revalidate';

export function afterAuthSignal(signal: AuthSignal): readonly AuthStep[] {
  switch (signal) {
    case 'whoami_401':
    case 'ticket_401':
      return ['tear_down_media', 'show_signed_out'];
    case 'socket_closed_1008':
      return ['revalidate'];
  }
}
