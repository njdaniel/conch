// Copy of the WSTicketV1 shape in pkg/schema/ws_ticket.go.
import { object, str, timestamp } from './validate';

// WSTicketV1 is the body of the 201 reply to the ticket request. The ticket
// is single use and lives 30 seconds; it is a secret and is never shown.
export interface WSTicketV1 {
  ticket: string;
  expires_at: string;
}

export const wsTicketV1 = object<WSTicketV1>({
  ticket: str,
  expires_at: timestamp,
});
