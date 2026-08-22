import { useMutation } from "@tanstack/react-query";
import { toast } from "sonner";
import { sendReaction } from "@/services/calls";

export const useSendReaction = () =>
  useMutation({
    mutationFn: (vars: { sid: string; callId: string; emoji: string }) =>
      sendReaction(vars.sid, vars.callId, vars.emoji),
    onError: (e: Error) => toast.error(e.message),
  });
