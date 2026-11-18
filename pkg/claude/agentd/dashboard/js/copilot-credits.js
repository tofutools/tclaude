// copilot-credits.js -- GitHub Copilot's premium-request allowance in dollars.
//
// GitHub prices one AI credit (AIC) at $0.01, the same rate as
// copilotGrossUSDPerCredit in pkg/claude/harness/copilot_cost.go. Every
// dollar figure made from AIC goes through here, so the header and the quota
// page cannot disagree.
import { fmtUSD } from './costs-model.js';

export const USD_PER_AIC = 0.01;

export function aicToUSD(aic) {
  const value = Number(aic);
  return Number.isFinite(value) && value > 0 ? value * USD_PER_AIC : 0;
}

export function fmtAicUSD(aic) {
  return fmtUSD(aicToUSD(aic));
}
