// Copy of the Error shape in pkg/schema/message.go: the body of every refusal.
import { int, object, optional, str } from './validate';

export interface ErrorV0 {
  code: string;
  message: string;
  retry_after_seconds?: number;
}

export const errorV0 = object<ErrorV0>({
  code: str,
  message: str,
  retry_after_seconds: optional(int),
});
