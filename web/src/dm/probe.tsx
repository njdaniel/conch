import { useDm } from './DmContext';

// DmContextProbe shows which conversation the inbox has open, for tests of the
// pieces that choose one (a card's Message action, an inbox row).
export function DmContextProbe() {
  const dm = useDm();
  return <output data-testid="selected">{dm?.inbox.state.selected === null || dm === null ? '' : String(dm.inbox.state.selected)}</output>;
}
