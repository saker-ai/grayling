import type { ReactElement } from 'react';
import { useTranslation } from 'react-i18next';

// ZoneNav ports renderExpZoneNav (exp_workbench.js ~417): the five
// deep-linkable zone buttons (four zones + the settings & audit tab). The
// active button carries aria-current; selection routes via onSelect so every
// zone keeps its own hash deep link.
export const ZONE_TABS = ['summary', 'health', 'evidence', 'rollout', 'settings'] as const;
export type ZoneTab = (typeof ZONE_TABS)[number];

const ZONE_LABEL_KEYS: Record<ZoneTab, string> = {
  summary: 'exp.zone.summary',
  health: 'exp.zone.health',
  evidence: 'exp.zone.evidence',
  rollout: 'exp.zone.rollout',
  settings: 'exp.tab.settings',
};

export function ZoneNav({
  active,
  onSelect,
}: {
  active: ZoneTab;
  onSelect: (tab: ZoneTab) => void;
}): ReactElement {
  const { t } = useTranslation();
  return (
    <nav className="exp-zone-nav" id="exp-zone-nav" role="tablist" aria-label={t('exp.zones.nav')}>
      {ZONE_TABS.map(name => {
        const isActive = name === active;
        return (
          <button
            key={name}
            type="button"
            role="tab"
            id={'exp-tab-' + name}
            className={'btn exp-zone-tab' + (isActive ? ' active' : '')}
            data-exp-tab={name}
            aria-current={isActive ? 'true' : undefined}
            onClick={() => {
              if (!isActive) onSelect(name);
            }}
          >
            {t(ZONE_LABEL_KEYS[name])}
          </button>
        );
      })}
    </nav>
  );
}
