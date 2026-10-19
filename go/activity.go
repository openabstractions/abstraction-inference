package inference

// modelActivity is a short-lived reservation for one model admission. Bounded
// Start calls reserve caller capacity in the same object; Embed reserves only
// model activity. All fields and pending counts are protected by Provider.mu.
type modelActivity struct {
	model    string
	subject  Subject
	capacity bool
	active   bool
}

// reserveModelActivity publishes an admission before the provider hook can do
// work outside the inference provider. It never holds Provider.mu across that
// work, which may load an engine or wait for a resource lease.
func (p *Provider) reserveModelActivity(model string, subject *Subject) (*modelActivity, string) {
	if model == "" {
		return nil, "model"
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ctx.Err() != nil {
		return nil, "provider:closed"
	}
	if subject != nil {
		active := p.pendingSubjects[*subject]
		for _, op := range p.ops {
			if op.subject == *subject && !op.isEnded() {
				active++
			}
		}
		if active >= p.cfg.MaxOperations {
			return nil, "capacity"
		}
		p.pendingSubjects[*subject]++
	}
	p.pendingModels[model]++
	activity := &modelActivity{model: model, active: true}
	if subject != nil {
		activity.subject, activity.capacity = *subject, true
	}
	return activity, ""
}

func (p *Provider) releaseModelActivity(activity *modelActivity) {
	if activity == nil {
		return
	}
	p.mu.Lock()
	p.releaseModelActivityLocked(activity)
	p.mu.Unlock()
}

// releaseModelActivityLocked consumes one reservation exactly once. Callers
// hold Provider.mu so an operation can replace its reservation atomically.
func (p *Provider) releaseModelActivityLocked(activity *modelActivity) {
	if activity == nil || !activity.active {
		return
	}
	activity.active = false
	if activity.capacity {
		if count := p.pendingSubjects[activity.subject]; count > 1 {
			p.pendingSubjects[activity.subject] = count - 1
		} else {
			delete(p.pendingSubjects, activity.subject)
		}
	}
	if count := p.pendingModels[activity.model]; count > 1 {
		p.pendingModels[activity.model] = count - 1
	} else {
		delete(p.pendingModels, activity.model)
	}
}
